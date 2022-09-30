// Copyright 2022 Northern.tech AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package azblob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"time"

	"github.com/mendersoftware/deployments/model"
	"github.com/mendersoftware/deployments/storage"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

const (
	headerBlobType             = "x-ms-blob-type"
	headerMSContentDisposition = "x-ms-blob-content-disposition"

	blobTypeBlock = "BlockBlob"
)

type client struct {
	*azblob.ContainerClient
	fileSuffix  *string
	contentType *string
	bufferSize  int
}

type SharedKeyCredentials struct {
	AccountName string
	AccountKey  string

	URI *string // Optional
}

func New(ctx context.Context, bucket string, opts ...*Options) (storage.ObjectStorage, error) {
	var (
		err error
		cc  *azblob.ContainerClient
	)
	opt := NewOptions(opts...)
	if opt.ConnectionString != nil {
		cc, err = azblob.NewContainerClientFromConnectionString(
			*opt.ConnectionString, bucket, &azblob.ClientOptions{},
		)
		if err != nil {
			return nil, err
		}
	} else if sk := opt.SharedKey; sk != nil {
		var cred *azblob.SharedKeyCredential
		cred, err = azblob.NewSharedKeyCredential(sk.AccountName, sk.AccountKey)
		if err != nil {
			return nil, err
		}
		var containerURI string
		if sk.URI != nil {
			containerURI = *sk.URI
		} else {
			containerURI = fmt.Sprintf(
				"https://%s.blob.core.windows.net/%s",
				cred.AccountName(),
				bucket,
			)
		}
		cc, err = azblob.NewContainerClientWithSharedKey(
			containerURI,
			cred,
			&azblob.ClientOptions{},
		)
	}
	if err != nil {
		return nil, err
	}
	objectStorage := &client{
		ContainerClient: cc,
		fileSuffix:      opt.FilenameSuffix,
		contentType:     opt.ContentType,
	}
	if err := objectStorage.HealthCheck(ctx); err != nil {
		return nil, err
	}
	return objectStorage, nil
}

func (c *client) HealthCheck(ctx context.Context) error {
	_, err := c.ContainerClient.GetProperties(ctx, &azblob.ContainerGetPropertiesOptions{})
	if err != nil {
		return OpError{
			Op:     OpHealthCheck,
			Reason: err,
		}
	}
	return nil
}

func (c *client) PutObject(
	ctx context.Context,
	objectPath string,
	src io.Reader,
) error {
	bc, err := c.ContainerClient.NewBlockBlobClient(objectPath)
	if err != nil {
		return OpError{
			Op:      OpPutObject,
			Message: "failed to initialize blob client",
			Reason:  err,
		}
	}
	var blobOpts = azblob.UploadStreamOptions{
		HTTPHeaders: &azblob.BlobHTTPHeaders{
			BlobContentType: c.contentType,
		},
	}
	if c.fileSuffix != nil {
		filename := path.Base(objectPath) + *c.fileSuffix
		disp := fmt.Sprintf(
			`attachment; filename="%s"`, filename,
		)
		blobOpts.HTTPHeaders.BlobContentDisposition = &disp
	}
	blobOpts.BufferSize = c.bufferSize
	_, err = bc.UploadStream(ctx, src, blobOpts)
	if err != nil {
		return OpError{
			Op:      OpPutObject,
			Message: "failed to upload object to blob",
			Reason:  err,
		}
	}
	return err
}

func (c *client) DeleteObject(
	ctx context.Context,
	path string,
) error {
	bc, err := c.ContainerClient.NewBlockBlobClient(path)
	if err != nil {
		return OpError{
			Op:      OpDeleteObject,
			Message: "failed to initialize blob client",
			Reason:  err,
		}
	}
	_, err = bc.Delete(ctx, &azblob.BlobDeleteOptions{
		DeleteSnapshots: azblob.DeleteSnapshotsOptionTypeInclude.ToPtr(),
	})
	var storageErr *azblob.StorageError
	if errors.As(err, &storageErr) {
		if storageErr.ErrorCode == azblob.StorageErrorCodeBlobNotFound {
			err = storage.ErrObjectNotFound
		}
	}
	if err != nil {
		return OpError{
			Op:      OpDeleteObject,
			Message: "failed to delete object",
			Reason:  err,
		}
	}
	return nil
}

func (c *client) StatObject(
	ctx context.Context,
	path string,
) (*storage.ObjectInfo, error) {
	bc, err := c.ContainerClient.NewBlockBlobClient(path)
	if err != nil {
		return nil, OpError{
			Op:      OpStatObject,
			Message: "failed to initialize blob client",
			Reason:  err,
		}
	}
	rsp, err := bc.GetProperties(ctx, &azblob.BlobGetPropertiesOptions{})
	var storageErr *azblob.StorageError
	if errors.As(err, &storageErr) {
		if storageErr.ErrorCode == azblob.StorageErrorCodeBlobNotFound {
			err = storage.ErrObjectNotFound
		}
	}
	if err != nil {
		return nil, OpError{
			Op:      OpStatObject,
			Message: "failed to retrieve object properties",
			Reason:  err,
		}
	}
	return &storage.ObjectInfo{
		Path:         path,
		LastModified: rsp.LastModified,
		Size:         rsp.ContentLength,
	}, nil
}

func buildSignedURL(
	blobURL string,
	SASParams azblob.SASQueryParameters,
) (string, error) {
	baseURL, err := url.Parse(blobURL)
	if err != nil {
		return "", err
	}
	qSAS, err := url.ParseQuery(SASParams.Encode())
	if err != nil {
		return "", err
	}
	q := baseURL.Query()
	for key, values := range qSAS {
		for _, value := range values {
			q.Add(key, value)
		}
	}
	baseURL.RawQuery = q.Encode()
	return baseURL.String(), nil
}

func (c *client) GetRequest(
	ctx context.Context,
	path string,
	duration time.Duration,
) (*model.Link, error) {
	// Check if object exists
	_, err := c.StatObject(ctx, path)
	if err != nil {
		return nil, err
	}
	bc, err := c.ContainerClient.NewBlobClient(path)
	if err != nil {
		return nil, OpError{
			Op:      OpGetRequest,
			Message: "failed to initialize blob client",
			Reason:  err,
		}
	}
	now := time.Now().UTC()
	exp := now.Add(duration)
	qParams, err := bc.GetSASToken(azblob.BlobSASPermissions{Read: true}, now, exp)
	if err != nil {
		return nil, OpError{
			Op:      OpGetRequest,
			Message: "failed to generate SAS token",
			Reason:  err,
		}
	}
	uri, err := buildSignedURL(bc.URL(), qParams)
	if err != nil {
		return nil, OpError{
			Op:      OpGetRequest,
			Message: "failed to create pre-signed URL",
			Reason:  err,
		}
	}
	return &model.Link{
		Uri:    uri,
		Expire: exp,
		Method: http.MethodGet,
	}, nil
}

func (c *client) DeleteRequest(
	ctx context.Context,
	path string,
	duration time.Duration,
) (*model.Link, error) {
	bc, err := c.ContainerClient.NewBlobClient(path)
	if err != nil {
		return nil, OpError{
			Op:      OpDeleteRequest,
			Message: "failed to initialize blob client",
			Reason:  err,
		}
	}
	now := time.Now().UTC()
	exp := now.Add(duration)
	qParams, err := bc.GetSASToken(azblob.BlobSASPermissions{Delete: true}, now, exp)
	if err != nil {
		return nil, OpError{
			Op:      OpDeleteRequest,
			Message: "failed to generate SAS token",
			Reason:  err,
		}
	}
	uri, err := buildSignedURL(bc.URL(), qParams)
	if err != nil {
		return nil, OpError{
			Op:      OpDeleteRequest,
			Message: "failed to create pre-signed URL",
			Reason:  err,
		}
	}
	return &model.Link{
		Uri:    uri,
		Expire: exp,
		Method: http.MethodDelete,
	}, nil
}

func (c *client) PutRequest(
	ctx context.Context,
	objectPath string,
	duration time.Duration,
) (*model.Link, error) {
	bc, err := c.ContainerClient.NewBlobClient(objectPath)
	if err != nil {
		return nil, OpError{
			Op:      OpPutRequest,
			Message: "failed to initialize blob client",
			Reason:  err,
		}
	}
	now := time.Now().UTC()
	exp := now.Add(duration)
	qParams, err := bc.GetSASToken(azblob.BlobSASPermissions{
		Create: true,
		Write:  true,
	}, now, exp)
	if err != nil {
		return nil, OpError{
			Op:      OpPutRequest,
			Message: "failed to generate SAS token",
			Reason:  err,
		}
	}
	uri, err := buildSignedURL(bc.URL(), qParams)
	if err != nil {
		return nil, OpError{
			Op:      OpPutRequest,
			Message: "failed to create pre-signed URL",
			Reason:  err,
		}
	}
	hdrs := map[string]string{
		headerBlobType: blobTypeBlock,
	}
	if c.fileSuffix != nil {
		filename := path.Base(objectPath) + *c.fileSuffix
		hdrs[headerMSContentDisposition] = fmt.Sprintf(
			`attachment; filename="%s"`, filename,
		)
	}
	return &model.Link{
		Uri:    uri,
		Expire: exp,
		Method: http.MethodPut,
		Header: hdrs,
	}, nil
}