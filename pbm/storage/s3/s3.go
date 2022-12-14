package s3

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/minio/minio-go"
	"github.com/pkg/errors"

	"github.com/percona/percona-backup-mongodb/pbm/storage"
)

const (
	// GCSEndpointURL is the endpoint url for Google Clound Strage service
	GCSEndpointURL = "storage.googleapis.com"

	defaultS3Region = "us-east-1"

	downloadChuckSize = 10 << 20 // 10Mb
	downloadRetries   = 10
)

type Conf struct {
	Provider             S3Provider  `bson:"provider,omitempty" json:"provider,omitempty" yaml:"provider,omitempty"`
	Region               string      `bson:"region" json:"region" yaml:"region"`
	EndpointURL          string      `bson:"endpointUrl,omitempty" json:"endpointUrl" yaml:"endpointUrl,omitempty"`
	Bucket               string      `bson:"bucket" json:"bucket" yaml:"bucket"`
	Prefix               string      `bson:"prefix,omitempty" json:"prefix,omitempty" yaml:"prefix,omitempty"`
	Credentials          Credentials `bson:"credentials" json:"credentials,omitempty" yaml:"credentials"`
	ServerSideEncryption *AWSsse     `bson:"serverSideEncryption,omitempty" json:"serverSideEncryption,omitempty" yaml:"serverSideEncryption,omitempty"`
	UploadPartSize       int         `bson:"uploadPartSize,omitempty" json:"uploadPartSize,omitempty" yaml:"uploadPartSize,omitempty"`
}

type AWSsse struct {
	SseAlgorithm string `bson:"sseAlgorithm" json:"sseAlgorithm" yaml:"sseAlgorithm"`
	KmsKeyID     string `bson:"kmsKeyID" json:"kmsKeyID" yaml:"kmsKeyID"`
}

func (c *Conf) Cast() error {
	if c.Region == "" {
		c.Region = defaultS3Region
	}
	if c.Provider == S3ProviderUndef {
		c.Provider = S3ProviderAWS
		if c.EndpointURL != "" {
			eu, err := url.Parse(c.EndpointURL)
			if err != nil {
				return errors.Wrap(err, "parse EndpointURL")
			}
			if eu.Host == GCSEndpointURL {
				c.Provider = S3ProviderGCS
			}
		}
	}

	return nil
}

type Credentials struct {
	AccessKeyID     string `bson:"access-key-id" json:"access-key-id,omitempty" yaml:"access-key-id,omitempty"`
	SecretAccessKey string `bson:"secret-access-key" json:"secret-access-key,omitempty" yaml:"secret-access-key,omitempty"`
	Vault           struct {
		Server string `bson:"server" json:"server,omitempty" yaml:"server"`
		Secret string `bson:"secret" json:"secret,omitempty" yaml:"secret"`
		Token  string `bson:"token" json:"token,omitempty" yaml:"token"`
	} `bson:"vault" json:"vault" yaml:"vault,omitempty"`
}

type S3Provider string

const (
	S3ProviderUndef S3Provider = ""
	S3ProviderAWS   S3Provider = "aws"
	S3ProviderGCS   S3Provider = "gcs"
)

type S3 struct {
	opts Conf
}

func New(opts Conf) (*S3, error) {
	err := opts.Cast()
	if err != nil {
		return nil, errors.Wrap(err, "cast options")
	}

	return &S3{
		opts: opts,
	}, nil
}

const defaultPartSize = 10 * 1024 * 1024 // 10Mb

func (s *S3) Save(name string, data io.Reader, sizeb int) error {
	switch s.opts.Provider {
	default:
		awsSession, err := s.session()
		if err != nil {
			return errors.Wrap(err, "create AWS session")
		}
		cc := runtime.NumCPU() / 2
		if cc == 0 {
			cc = 1
		}

		uplInput := &s3manager.UploadInput{
			Bucket: aws.String(s.opts.Bucket),
			Key:    aws.String(path.Join(s.opts.Prefix, name)),
			Body:   data,
		}

		sse := s.opts.ServerSideEncryption
		if sse != nil && sse.SseAlgorithm != "" {
			uplInput.ServerSideEncryption = aws.String(sse.SseAlgorithm)
			if sse.SseAlgorithm == s3.ServerSideEncryptionAwsKms {
				uplInput.SSEKMSKeyId = aws.String(sse.KmsKeyID)
			}
		}

		// MaxUploadParts is 1e4 so with PartSize 10Mb the max allowed file size
		// would be ~ 97.6Gb. Hence if the file size is bigger we're enlarging PartSize
		// so PartSize * MaxUploadParts could fit the file.
		// If calculated PartSize is smaller than the default we leave the default.
		// If UploadPartSize option was set we use it instead of the default. Even
		// with the UploadPartSize set the calculated PartSize woulbe used if it's bigger.
		partSize := defaultPartSize
		if s.opts.UploadPartSize > 0 {
			if s.opts.UploadPartSize < int(s3manager.MinUploadPartSize) {
				s.opts.UploadPartSize = int(s3manager.MinUploadPartSize)
			}

			partSize = s.opts.UploadPartSize
		}
		if sizeb > 0 {
			ps := sizeb / s3manager.MaxUploadParts * 9 / 10 // shed 10% just in case
			if ps > partSize {
				partSize = ps
			}
		}
		// TODO: needs to be pbm.Logger
		fmt.Fprintf(os.Stderr, "%s [INFO] s3.uploadPartSize is set to %d (~%dMb)\n", time.Now().Format("2006-01-02T15:04:05.000-0700"), partSize, partSize>>20)

		_, err = s3manager.NewUploader(awsSession, func(u *s3manager.Uploader) {
			u.MaxUploadParts = s3manager.MaxUploadParts
			u.PartSize = int64(partSize) // 10MB part size
			u.LeavePartsOnError = true   // Don't delete the parts if the upload fails.
			u.Concurrency = cc
		}).Upload(uplInput)
		return errors.Wrap(err, "upload to S3")
	case S3ProviderGCS:
		// using minio client with GCS because it
		// allows to disable chuncks muiltipertition for upload
		mc, err := minio.NewWithRegion(GCSEndpointURL, s.opts.Credentials.AccessKeyID, s.opts.Credentials.SecretAccessKey, true, s.opts.Region)
		if err != nil {
			return errors.Wrap(err, "NewWithRegion")
		}
		_, err = mc.PutObject(s.opts.Bucket, path.Join(s.opts.Prefix, name), data, -1, minio.PutObjectOptions{})
		return errors.Wrap(err, "upload to GCS")
	}
}

func (s *S3) Files(suffix string) ([][]byte, error) {
	s3s, err := s.s3session()
	if err != nil {
		return nil, errors.Wrap(err, "AWS session")
	}

	lparams := &s3.ListObjectsInput{
		Bucket:    aws.String(s.opts.Bucket),
		Delimiter: aws.String("/"),
	}
	if s.opts.Prefix != "" {
		lparams.Prefix = aws.String(s.opts.Prefix)
		if s.opts.Prefix[len(s.opts.Prefix)-1] != '/' {
			*lparams.Prefix += "/"
		}
	}

	var bcps [][]byte
	var berr error
	err = s3s.ListObjectsPages(lparams,
		func(page *s3.ListObjectsOutput, lastPage bool) bool {
			for _, o := range page.Contents {
				name := aws.StringValue(o.Key)
				if strings.HasSuffix(name, suffix) {
					s3obj, err := s3s.GetObject(&s3.GetObjectInput{
						Bucket: aws.String(s.opts.Bucket),
						Key:    aws.String(name),
					})
					if err != nil {
						berr = errors.Wrapf(err, "get object '%s'", name)
						return false
					}

					sse := s.opts.ServerSideEncryption
					if sse != nil && sse.SseAlgorithm != "" {
						s3obj.ServerSideEncryption = aws.String(sse.SseAlgorithm)
						if sse.SseAlgorithm == s3.ServerSideEncryptionAwsKms {
							s3obj.SSEKMSKeyId = aws.String(sse.KmsKeyID)
						}
					}

					b, err := ioutil.ReadAll(s3obj.Body)
					if err != nil {
						berr = errors.Wrapf(err, "read object '%s'", name)
						return false
					}
					bcps = append(bcps, b)
				}
			}
			return true
		})

	if err != nil {
		return nil, errors.Wrap(err, "get backup list")
	}

	if berr != nil {
		return nil, errors.Wrap(berr, "metadata")
	}

	return bcps, nil
}

func (s *S3) List(prefix string) ([]string, error) {
	s3s, err := s.s3session()
	if err != nil {
		return nil, errors.Wrap(err, "AWS session")
	}

	lparams := &s3.ListObjectsInput{
		Bucket: aws.String(s.opts.Bucket),
	}
	if s.opts.Prefix != "" {
		lparams.Prefix = aws.String(s.opts.Prefix)
		if s.opts.Prefix[len(s.opts.Prefix)-1] != '/' {
			*lparams.Prefix += "/"
		}
	}

	if aws.StringValue(lparams.Prefix) != "" || prefix != "" {
		lparams.Prefix = aws.String(path.Join(aws.StringValue(lparams.Prefix), prefix))
	}

	var files []string
	err = s3s.ListObjectsPages(lparams,
		func(page *s3.ListObjectsOutput, lastPage bool) bool {
			for _, o := range page.Contents {
				f := aws.StringValue(o.Key)
				f = strings.TrimPrefix(f, aws.StringValue(lparams.Prefix))
				if len(f) == 0 {
					continue
				}
				if f[0] == '/' {
					f = f[1:]
				}
				files = append(files, f)
			}
			return true
		})

	if err != nil {
		return nil, errors.Wrap(err, "get backup list")
	}

	return files, nil
}

func (s *S3) CheckFile(name string) error {
	s3s, err := s.s3session()
	if err != nil {
		return errors.Wrap(err, "AWS session")
	}

	h, err := s3s.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(s.opts.Bucket),
		Key:    aws.String(path.Join(s.opts.Prefix, name)),
	})
	if err != nil {
		return errors.Wrap(err, "get S3 object header")
	}

	if aws.Int64Value(h.ContentLength) == 0 {
		return errors.New("file empty")
	}
	if aws.BoolValue(h.DeleteMarker) {
		return errors.New("file has delete marker")
	}

	return nil
}

type (
	errGetObj  error
	errReadObj error
)

type partReader struct {
	fname string
	sess  *s3.S3
	l     *llog
	opts  *Conf
	n     int64
	tsize int64
	buf   []byte
}

func (s *S3) newPartReader(fname string) *partReader {
	return &partReader{
		l:     &llog{os.Stderr, "2006-01-02T15:04:05.000-0700"},
		buf:   make([]byte, downloadChuckSize),
		opts:  &s.opts,
		fname: fname,
		tsize: -2,
	}
}

func (pr *partReader) setSession(s *s3.S3) {
	s.Client.Config.HTTPClient.Timeout = time.Second * 30
	pr.sess = s
}

func (pr *partReader) tryNext(w io.Writer) (n int64, err error) {
	for i := 0; i < downloadRetries; i++ {
		n, err = pr.writeNext(w)

		if err == nil || err == io.EOF {
			return n, err
		}

		switch err.(type) {
		case errGetObj:
			return n, err
		}

		pr.l.Warning("failed to download chunk %d-%d", pr.n, pr.n+downloadChuckSize-1)
	}

	return 0, errors.Wrapf(err, "failed to download chunk %d-%d (of %d) after %d retries", pr.n, pr.n+downloadChuckSize-1, pr.tsize, downloadRetries)
}

func (pr *partReader) writeNext(w io.Writer) (n int64, err error) {
	s3obj, err := pr.sess.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(pr.opts.Bucket),
		Key:    aws.String(path.Join(pr.opts.Prefix, pr.fname)),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", pr.n, pr.n+downloadChuckSize-1)),
	})

	if err != nil {
		// if object size is undefined, we would read
		// until HTTP code 416 (Requested Range Not Satisfiable)
		var er awserr.RequestFailure
		if errors.As(err, &er) && er.StatusCode() == http.StatusRequestedRangeNotSatisfiable {
			return 0, io.EOF
		}
		pr.l.Warning("errGetObj Err: %v", err)
		return 0, errGetObj(err)
	}
	if pr.tsize == -2 {
		pr.setSize(s3obj)
	}

	if pr.opts.ServerSideEncryption != nil {
		sse := pr.opts.ServerSideEncryption

		s3obj.ServerSideEncryption = aws.String(sse.SseAlgorithm)
		if sse.SseAlgorithm == s3.ServerSideEncryptionAwsKms {
			s3obj.SSEKMSKeyId = aws.String(sse.KmsKeyID)
		}
	}

	n, err = io.CopyBuffer(w, s3obj.Body, pr.buf)
	s3obj.Body.Close()

	pr.n += n

	// we don't care about the error if we've read the entire object
	if pr.tsize >= 0 && pr.n >= pr.tsize {
		return 0, io.EOF
	}

	if err != nil {
		pr.l.Warning("errReadObj Err: %v", err)
		return n, errReadObj(err)
	}

	return n, nil
}

func (pr *partReader) setSize(o *s3.GetObjectOutput) {
	pr.tsize = -1
	if o.ContentRange == nil {
		if o.ContentLength != nil {
			pr.tsize = *o.ContentLength
		}
		return
	}

	rng := strings.Split(*o.ContentRange, "/")
	if len(rng) < 2 || rng[1] == "*" {
		return
	}

	size, err := strconv.ParseInt(rng[1], 10, 64)
	if err != nil {
		pr.l.Warning("unable to parse object size from %s: %v", rng[1], err)
		return
	}

	pr.tsize = size
}

// SourceReader reads object with the given name from S3
// and pipes its data to the returned io.ReadCloser.
//
// It uses partReader to download the object by chunks (`downloadChuckSize`).
// In case of error, it would retry get the next bytes up to `downloadRetries` times.
// If it fails to do so or connection error happened, it recreates the session
// and tries again up to `downloadRetries` times.
func (s *S3) SourceReader(name string) (io.ReadCloser, error) {
	s3s, err := s.s3session()
	if err != nil {
		return nil, errors.Wrap(err, "AWS session")
	}

	pr := s.newPartReader(name)
	pr.setSession(s3s)

	r, w := io.Pipe()

	go func() {
		defer w.Close()

		slog := &llog{os.Stderr, "2006-01-02T15:04:05.000-0700"}
	Loop:
		for {
			for i := 0; i < downloadRetries; i++ {
				_, err := pr.tryNext(w)
				if err == nil {
					continue Loop
				}
				if err == io.EOF {
					return
				}
				if errors.Is(err, io.ErrClosedPipe) {
					slog.Info("reader closed pipe, stopping download")
					return
				}

				slog.Warning("got %v, try to reconnect in %v", err, time.Second*time.Duration(i+1))
				time.Sleep(time.Second * time.Duration(i+1))
				s3s, err := s.s3session()
				if err != nil {
					slog.Warning("recreate session")
					continue
				}
				pr.setSession(s3s)
				slog.Info("session recreated, resuming download")
			}
			slog.Error("download '%s/%s' file from S3: %v", s.opts.Bucket, name, err)
			return
		}
	}()

	return r, nil
}

type llog struct {
	dst     io.Writer
	tformat string
}

func (l *llog) print(level, msg string, args ...interface{}) {
	fmt.Fprintf(l.dst, "%s [%s] %s\n", time.Now().Format(l.tformat), level, fmt.Sprintf(msg, args...))
}

func (l *llog) Error(msg string, args ...interface{}) {
	l.print("ERROR", msg, args...)
}

func (l *llog) Warning(msg string, args ...interface{}) {
	l.print("Warning", msg, args...)
}

func (l *llog) Info(msg string, args ...interface{}) {
	l.print("INFO", msg, args...)
}

// Delete deletes given file.
// It returns storage.ErrNotExist if a file isn't exists
func (s *S3) Delete(name string) error {
	s3s, err := s.s3session()
	if err != nil {
		return errors.Wrap(err, "AWS session")
	}
	_, err = s3s.DeleteObject(&s3.DeleteObjectInput{
		Bucket: aws.String(s.opts.Bucket),
		Key:    aws.String(path.Join(s.opts.Prefix, name)),
	})

	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case s3.ErrCodeNoSuchKey:
				return storage.ErrNotExist
			}
		}
		return errors.Wrapf(err, "delete '%s/%s' file from S3", s.opts.Bucket, name)
	}

	return nil
}

func (s *S3) s3session() (*s3.S3, error) {
	sess, err := s.session()
	if err != nil {
		return nil, errors.Wrap(err, "create aws session")
	}

	return s3.New(sess), nil
}

func (s *S3) session() (*session.Session, error) {
	return session.NewSession(&aws.Config{
		Region:   aws.String(s.opts.Region),
		Endpoint: aws.String(s.opts.EndpointURL),
		Credentials: credentials.NewStaticCredentials(
			s.opts.Credentials.AccessKeyID,
			s.opts.Credentials.SecretAccessKey,
			"",
		),
		S3ForcePathStyle: aws.Bool(true),
	})
}
