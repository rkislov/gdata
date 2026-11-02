// Copyright 2026 Кислов Роман Сергеевич
// SPDX-License-Identifier: Apache-2.0
package service

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gdata/srk/internal/config"
	"gdata/srk/internal/model"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

func s3Client(p config.S3Profile) (*minio.Client, error) {
	b, e := os.ReadFile(p.CredentialFile)
	if e != nil {
		return nil, e
	}
	var cred struct {
		Access  string `json:"access_key"`
		Secret  string `json:"secret_key"`
		Session string `json:"session_token"`
	}
	if json.Unmarshal(b, &cred) != nil || cred.Access == "" || cred.Secret == "" {
		return nil, errors.New("invalid S3 credential file")
	}
	roots, e := x509.SystemCertPool()
	if e != nil {
		roots = x509.NewCertPool()
	}
	if p.CAFile != "" {
		ca, e := os.ReadFile(p.CAFile)
		if e != nil || !roots.AppendCertsFromPEM(ca) {
			return nil, errors.New("invalid S3 CA")
		}
	}
	u, _ := url.Parse(p.Endpoint)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(cred.Access, cred.Secret, cred.Session), Secure: true, Region: p.Region, Transport: noS3Redirect{transport}, BucketLookup: minio.BucketLookupPath, MaxRetries: 1})
}
func s3Error(e error) error {
	if e == nil {
		return nil
	}
	response := minio.ToErrorResponse(e)
	if response.Code != "" {
		return fmt.Errorf("S3 operation failed (%s)", response.Code)
	}
	return errors.New("S3 operation failed; check TLS, connectivity and permissions")
}
func s3Listing(ctx context.Context, c *minio.Client, p config.S3Profile) (map[string]string, error) {
	out := map[string]string{}
	for item := range c.ListObjects(ctx, p.Bucket, minio.ListObjectsOptions{Prefix: p.Prefix, Recursive: true}) {
		if item.Err != nil {
			return nil, s3Error(item.Err)
		}
		if len(out) >= 10000 {
			return nil, errors.New("S3 object count exceeds 10000 per backup")
		}
		out[item.Key] = fmt.Sprintf("%s/%d", item.ETag, item.Size)
	}
	return out, nil
}
func (a *Agent) downloadS3(ctx context.Context, v model.Execute, p config.S3Profile, dir string, meta *model.DataBackup) error {
	client, e := s3Client(p)
	if e != nil {
		return e
	}
	before, e := s3Listing(ctx, client, p)
	if e != nil {
		return e
	}
	meta.Bucket = p.Bucket
	meta.Prefix = p.Prefix
	var total int64
	index := 0
	for item := range client.ListObjects(ctx, p.Bucket, minio.ListObjectsOptions{Prefix: p.Prefix, Recursive: true}) {
		if item.Err != nil {
			return s3Error(item.Err)
		}
		if item.Size < 0 || item.Size > 5<<30 || item.Size > p.MaxBytes-total {
			return errors.New("S3 staging quota exceeded or object exceeds 5 GiB single-PUT restore bound")
		}
		total += item.Size
		info, e := client.StatObject(ctx, p.Bucket, item.Key, minio.StatObjectOptions{})
		if e != nil {
			return s3Error(e)
		}
		if info.Size != item.Size || info.ETag != item.ETag {
			return errors.New("S3 source changed during listing")
		}
		tags, e := client.GetObjectTagging(ctx, p.Bucket, item.Key, minio.GetObjectTaggingOptions{VersionID: info.VersionID})
		if e != nil {
			return s3Error(e)
		}
		opts := minio.GetObjectOptions{VersionID: info.VersionID}
		if e = opts.SetMatchETag(info.ETag); e != nil {
			return e
		}
		input, e := client.GetObject(ctx, p.Bucket, item.Key, opts)
		if e != nil {
			return s3Error(e)
		}
		relative := fmt.Sprintf("objects/%08d", index)
		index++
		if e = os.MkdirAll(filepath.Join(dir, "objects"), 0700); e != nil {
			input.Close()
			return e
		}
		out, e := os.OpenFile(filepath.Join(dir, relative), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			input.Close()
			return e
		}
		copied, err := io.Copy(out, io.LimitReader(input, item.Size+1))
		input.Close()
		syncErr := out.Sync()
		closeErr := out.Close()
		if err != nil {
			return s3Error(err)
		}
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
		if copied != item.Size {
			return errors.New("S3 object size changed")
		}
		digest, e := fileSHA(filepath.Join(dir, relative))
		if e != nil {
			return e
		}
		meta.Objects = append(meta.Objects, model.S3Object{File: relative, Key: item.Key, VersionID: info.VersionID, ETag: info.ETag, ContentType: info.ContentType, ContentEncoding: info.Metadata.Get("Content-Encoding"), CacheControl: info.Metadata.Get("Cache-Control"), Metadata: info.UserMetadata, Tags: tags.ToMap(), Bytes: item.Size, SHA256: digest})
		a.progressUpdate(v.RunID, model.Result{Phase: "s3-download", Files: index, TotalFiles: len(before), LogicalBytes: total, ProgressScope: "operation"})
	}
	after, e := s3Listing(ctx, client, p)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(before, after) || len(meta.Objects) != len(before) {
		return errors.New("S3 source changed during backup; retry with writes paused")
	}
	return writeDataIndex(dir, meta)
}
func (a *Agent) restoreS3(ctx context.Context, v model.Execute, p config.S3Profile, prefix, dir string, meta *model.DataBackup, res *model.Result) error {
	if prefix == "" || !strings.HasSuffix(prefix, "/") {
		return errors.New("S3 restore requires a distinct non-empty target prefix ending in /")
	}
	client, e := s3Client(p)
	if e != nil {
		return e
	}
	keyFor := func(o model.S3Object) string { return p.Prefix + prefix + strings.TrimPrefix(o.Key, meta.Prefix) }
	for _, obj := range meta.Objects {
		_, err := client.StatObject(ctx, p.Bucket, keyFor(obj), minio.StatObjectOptions{})
		code := minio.ToErrorResponse(err).Code
		if err == nil {
			return errors.New("S3 restore target already contains an object")
		}
		if code != "NoSuchKey" && code != "NoSuchObject" && code != "NotFound" {
			return s3Error(err)
		}
	}
	for _, obj := range meta.Objects {
		file := filepath.Join(dir, filepath.FromSlash(obj.File))
		digest, e := fileSHA(file)
		if e != nil {
			return e
		}
		if digest != obj.SHA256 {
			return errors.New("S3 restored object digest mismatch")
		}
		input, e := os.Open(file)
		if e != nil {
			return e
		}
		options := minio.PutObjectOptions{ContentType: obj.ContentType, ContentEncoding: obj.ContentEncoding, CacheControl: obj.CacheControl, UserMetadata: obj.Metadata, UserTags: obj.Tags, DisableMultipart: true, SendContentMd5: true}
		options.SetMatchETagExcept("*")
		_, e = client.PutObject(ctx, p.Bucket, keyFor(obj), input, obj.Bytes, options)
		input.Close()
		if e != nil {
			return fmt.Errorf("S3 restore stopped; successfully created objects are retained: %w", s3Error(e))
		}
		// Read back the new object independently of the upload acknowledgement.
		uploaded, e := client.GetObject(ctx, p.Bucket, keyFor(obj), minio.GetObjectOptions{})
		if e != nil {
			return s3Error(e)
		}
		check := sha256.New()
		count, err := io.Copy(check, io.LimitReader(uploaded, obj.Bytes+1))
		uploaded.Close()
		if err != nil {
			return s3Error(err)
		}
		if count != obj.Bytes || hex.EncodeToString(check.Sum(nil)) != obj.SHA256 {
			return errors.New("S3 target readback digest mismatch")
		}

		res.Phase = "s3-upload"
		a.progressUpdate(v.RunID, *res)
	}
	return nil
}

// Never follow a storage endpoint redirect with signed credentials.
type noS3Redirect struct{ http.RoundTripper }

func (t noS3Redirect) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, e := t.RoundTripper.RoundTrip(r)
	if e == nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, errors.New("S3 redirects are forbidden")
	}
	return resp, e
}
