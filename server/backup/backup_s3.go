package backup

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/cenkalti/backoff/v4"
	"github.com/juju/ratelimit"
	"github.com/mholt/archives"
	"golang.org/x/sync/errgroup"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/server/filesystem"
)

type S3Backup struct {
	Backup
}

var _ BackupInterface = (*S3Backup)(nil)

func NewS3(client remote.Client, uuid string, ignore string) *S3Backup {
	return &S3Backup{
		Backup{
			client:  client,
			Uuid:    uuid,
			Ignore:  ignore,
			adapter: S3BackupAdapter,
		},
	}
}

// Remove removes a backup from the system.
func (s *S3Backup) Remove() error {
	if err := s.validateIdentifier(); err != nil {
		return err
	}
	return os.Remove(s.Path())
}

// WithLogContext attaches additional context to the log output for this backup.
func (s *S3Backup) WithLogContext(c map[string]interface{}) {
	s.logContext = c
}

// Generate creates a new backup on the disk and uploads it to S3. If the
// remote upload cannot be completed, the archive is retained in the local
// backup directory and reported as a Wings backup instead.
func (s *S3Backup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
	if err := s.validateIdentifier(); err != nil {
		return nil, err
	}

	a := &filesystem.Archive{
		Filesystem: fsys,
		Ignore:     ignore,
	}

	s.log().WithField("path", s.Path()).Info("backup stage archive: creating archive for server")
	if err := a.Create(ctx, s.Path()); err != nil {
		return nil, err
	}
	s.log().Info("backup stage archive: archive created successfully")

	s.log().Info("backup stage s3: starting multipart upload")
	parts, err := s.generateRemoteRequest(ctx)
	if err != nil {
		s.log().WithError(err).WithField("path", s.Path()).Warn("backup stage s3: upload failed; starting local fallback")
		ad, detailsErr := s.Details(ctx, nil)
		if detailsErr != nil {
			return nil, errors.WrapIf(detailsErr, "backup: failed to get archive details for local fallback")
		}
		ad.Adapter, ad.RetainedLocally = LocalBackupAdapter, true
		s.log().WithField("path", s.Path()).Info("backup stage fallback: archive retained locally and ready to report as wings backup")
		return ad, nil
	}
	s.log().Info("backup stage s3: multipart upload completed successfully")

	// The remote copy is complete, so retain the existing behavior of removing
	// the temporary local archive. Do not do this in the fallback path above.
	defer func() {
		if err := s.Remove(); err != nil && !os.IsNotExist(err) {
			s.log().WithError(err).Warn("failed to remove temporary archive after S3 upload")
		}
	}()

	ad, err := s.Details(ctx, parts)
	if err != nil {
		return nil, errors.WrapIf(err, "backup: failed to get archive details after upload")
	}
	ad.Adapter = S3BackupAdapter
	return ad, nil
}

// Restore will read from the provided reader assuming that it is a gzipped
// tar reader. When a file is encountered in the archive the callback function
// will be triggered. If the callback returns an error the entire process is
// stopped, otherwise this function will run until all files have been written.
//
// This restoration uses a workerpool to use up to the number of CPUs available
// on the machine when writing files to the disk.
func (s *S3Backup) Restore(ctx context.Context, r io.Reader, callback RestoreCallback) error {
	reader := r
	// Steal the logic we use for making backups which will be applied when restoring
	// this specific backup. This allows us to prevent overloading the disk unintentionally.
	if writeLimit := int64(config.Get().System.Backups.WriteLimit * 1024 * 1024); writeLimit > 0 {
		reader = ratelimit.Reader(r, ratelimit.NewBucketWithRate(float64(writeLimit), writeLimit))
	}
	if err := format.Extract(ctx, reader, func(ctx context.Context, f archives.FileInfo) error {
		r, err := f.Open()
		if err != nil {
			return err
		}
		defer r.Close()

		return callback(f.NameInArchive, f.FileInfo, r)
	}); err != nil {
		return err
	}
	return nil
}

// Generates the remote S3 request and begins the upload.
func (s *S3Backup) generateRemoteRequest(ctx context.Context) ([]remote.BackupPart, error) {
	s.log().Debug("attempting to get size of backup...")
	size, err := s.Backup.Size()
	if err != nil {
		return nil, err
	}
	s.log().WithField("size", size).Debug("got size of backup")

	s.log().Debug("attempting to get S3 upload urls from Panel...")
	urls, err := s.client.GetBackupRemoteUploadURLs(context.Background(), s.Backup.Uuid, size)
	if err != nil {
		return nil, err
	}
	s.log().Debug("got S3 upload urls from the Panel")
	s.log().WithField("parts", len(urls.Parts)).Info("attempting to upload backup to s3 endpoint...")

	uploader := newS3FileUploader(s.Path(), s.log())
	uploadedParts := make([]remote.BackupPart, len(urls.Parts))
	concurrency := config.Get().System.Backups.S3UploadConcurrency
	if concurrency < 1 {
		concurrency = 1
	} else if concurrency > 4 {
		concurrency = 4
	}
	s.log().WithFields(log.Fields{"parts": len(urls.Parts), "concurrency": concurrency}).Info("uploading S3 backup parts")

	g, uploadCtx := errgroup.WithContext(ctx)
	semaphore := make(chan struct{}, concurrency)
	for i, part := range urls.Parts {
		i, part := i, part
		// Get the size for the current part.
		var partSize int64
		if i+1 < len(urls.Parts) {
			partSize = urls.PartSize
		} else {
			// This is the remaining size for the last part,
			// there is not a minimum size limit for the last part.
			partSize = size - (int64(i) * urls.PartSize)
		}

		offset := int64(i) * urls.PartSize
		g.Go(func() error {
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-uploadCtx.Done():
				return uploadCtx.Err()
			}

			etag, err := uploader.uploadPart(uploadCtx, i+1, part, offset, partSize)
			if err != nil {
				return err
			}
			uploadedParts[i] = remote.BackupPart{ETag: etag, PartNumber: i + 1}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	s.log().WithField("parts", len(urls.Parts)).Info("backup has been successfully uploaded")

	return uploadedParts, nil
}

type s3FileUploader struct {
	path   string
	client *http.Client
	logger *log.Entry
}

// newS3FileUploader returns a new file uploader instance.
func newS3FileUploader(path string, logger *log.Entry) *s3FileUploader {
	return &s3FileUploader{
		path: path,
		// We purposefully use a super high timeout on this request since we need to upload
		// a 5GB file. This assumes at worst a 10Mbps connection for uploading. While technically
		// you could go slower we're targeting mostly hosted servers that should have 100Mbps
		// connections anyways.
		client: &http.Client{
			Timeout: time.Hour * 2,
			// Garage uploads over this endpoint stall under HTTP/2. Keep multipart
			// transfers on HTTP/1.1, which also gives each concurrent part its own
			// TCP connection instead of multiplexing them onto one stalled stream.
			Transport: &http.Transport{
				ForceAttemptHTTP2: false,
				TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
			},
		},
		logger: logger,
	}
}

// backoff returns a new expoential backoff implementation using a context that
// will also stop the backoff if it is canceled.
func (fu *s3FileUploader) backoff(ctx context.Context) backoff.BackOffContext {
	b := backoff.NewExponentialBackOff()
	b.Multiplier = 2
	b.MaxElapsedTime = time.Minute

	return backoff.WithContext(b, ctx)
}

// uploadPart attempts to upload a given S3 file part to the S3 system. If a
// 5xx error is returned from the endpoint this will continue with an exponential
// backoff to try and successfully upload the part.
//
// Once uploaded the ETag is returned to the caller.
func (fu *s3FileUploader) uploadPart(ctx context.Context, partNumber int, part string, offset, size int64) (string, error) {
	startedAt := time.Now()
	logFields := log.Fields{"part_id": partNumber, "offset_bytes": offset, "size_bytes": size}
	fu.logger.WithFields(logFields).Info("backup part upload started")

	attempts := 0
	var etag string
	err := backoff.Retry(func() error {
		attempts++
		if attempts > 1 {
			fu.logger.WithFields(logFields).WithField("retry", attempts-1).Warn("retrying backup part upload")
		}

		// Every attempt opens a fresh descriptor and section reader. Reusing the
		// previous io.LimitReader would retry from its already-consumed position.
		file, err := os.Open(fu.path)
		if err != nil {
			return backoff.Permanent(errors.Wrap(err, "backup: could not open archive for S3 upload"))
		}
		defer file.Close()

		r, err := http.NewRequestWithContext(ctx, http.MethodPut, part, io.NopCloser(io.NewSectionReader(file, offset, size)))
		if err != nil {
			return backoff.Permanent(errors.Wrap(err, "backup: could not create request for S3"))
		}
		r.ContentLength = size
		r.Header.Set("Content-Length", strconv.FormatInt(size, 10))
		r.Header.Set("Content-Type", "application/x-gzip")

		res, err := fu.client.Do(r)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return backoff.Permanent(err)
			}
			// Don't use a permanent error here, if there is a temporary resolution error with
			// the URL due to DNS issues we want to keep re-trying.
			return errors.Wrap(err, "backup: S3 HTTP request failed")
		}
		_ = res.Body.Close()

		if res.StatusCode != http.StatusOK {
			err := errors.New(fmt.Sprintf("backup: failed to put S3 object: [HTTP/%d] %s", res.StatusCode, res.Status))
			// Only attempt a backoff retry if this error is because of a 5xx error from
			// the S3 endpoint. Any 4xx error should be treated as an error that a retry
			// would not fix.
			if res.StatusCode >= http.StatusInternalServerError {
				return err
			}
			return backoff.Permanent(err)
		}

		// Get the ETag from the uploaded part, this should be sent with the
		// CompleteMultipartUpload request.
		etag = res.Header.Get("ETag")

		return nil
	}, fu.backoff(ctx))
	duration := time.Since(startedAt)
	megabitsPerSecond := 0.0
	if duration > 0 {
		megabitsPerSecond = float64(size*8) / duration.Seconds() / 1_000_000
	}
	endFields := log.Fields{
		"part_id":                partNumber,
		"size_bytes":             size,
		"duration_ms":            duration.Milliseconds(),
		"megabits_per_second":    megabitsPerSecond,
		"retries":                attempts - 1,
	}
	if err != nil {
		if v, ok := err.(*backoff.PermanentError); ok {
			err = v.Unwrap()
		}
		fu.logger.WithFields(endFields).WithError(err).Warn("backup part upload ended unsuccessfully")
		return "", err
	}
	fu.logger.WithFields(endFields).Info("backup part upload completed")
	return etag, nil
}
