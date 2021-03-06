package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/graymeta/stow"
	"github.com/graymeta/stow/s3"
	"github.com/pivotal-cf/om/progress"
	"gopkg.in/go-playground/validator.v9"
)

//go:generate counterfeiter -o ./fakes/config_service.go --fake-name Config . Config
type Config interface {
	Config(name string) (string, bool)
	Set(name, value string)
}

type Stower interface {
	Dial(kind string, config Config) (stow.Location, error)
	Walk(container stow.Container, prefix string, pageSize int, fn stow.WalkFunc) error
}

type S3Configuration struct {
	Bucket          string `yaml:"bucket" validate:"required"`
	AccessKeyID     string `yaml:"access-key-id" validate:"required"`
	SecretAccessKey string `yaml:"secret-access-key" validate:"required"`
	RegionName      string `yaml:"region-name" validate:"required"`
	Endpoint        string `yaml:"endpoint"`
	DisableSSL      bool   `yaml:"disable-ssl"`
	EnableV2Signing bool   `yaml:"enable-v2-signing"`
	Path            string `yaml:"path"`
}

type S3Client struct {
	stower         Stower
	bucket         string
	Config         stow.Config
	progressWriter io.Writer
	path           string
}

func NewS3Client(stower Stower, config S3Configuration, progressWriter io.Writer) (*S3Client, error) {
	validate := validator.New()
	err := validate.Struct(config)
	if err != nil {
		return nil, err
	}

	disableSSL := strconv.FormatBool(config.DisableSSL)
	enableV2Signing := strconv.FormatBool(config.EnableV2Signing)
	stowConfig := stow.ConfigMap{
		s3.ConfigAccessKeyID: config.AccessKeyID,
		s3.ConfigSecretKey:   config.SecretAccessKey,
		s3.ConfigRegion:      config.RegionName,
		s3.ConfigEndpoint:    config.Endpoint,
		s3.ConfigDisableSSL:  disableSSL,
		s3.ConfigV2Signing:   enableV2Signing,
	}

	return &S3Client{
		stower:         stower,
		Config:         stowConfig,
		bucket:         config.Bucket,
		progressWriter: progressWriter,
		path:           config.Path,
	}, nil
}

func (s3 S3Client) GetAllProductVersions(slug string) ([]string, error) {
	files, err := s3.listFiles()
	if err != nil {
		return nil, err
	}

	productFileCompiledRegex := regexp.MustCompile(
		fmt.Sprintf(`^/?%s/?\[%s,(.*?)\]`,
			regexp.QuoteMeta(strings.Trim(s3.path, "/")),
			slug,
		),
	)

	var versions []string
	versionFound := make(map[string]bool)
	for _, fileName := range files {
		match := productFileCompiledRegex.FindStringSubmatch(fileName)
		if match != nil {
			version := match[1]
			if !versionFound[version] {
				versions = append(versions, version)
				versionFound[version] = true
			}
		}
	}

	if len(versions) == 0 {
		return nil, fmt.Errorf("no files matching pivnet-product-slug %s found", slug)
	}

	return versions, nil

}

func (s3 S3Client) GetLatestProductFile(slug, version, glob string) (*FileArtifact, error) {
	files, err := s3.listFiles()
	if err != nil {
		return nil, err
	}

	validFile := regexp.MustCompile(
		fmt.Sprintf(`^/?%s/?\[%s,%s\]`,
			regexp.QuoteMeta(strings.Trim(s3.path, "/")),
			slug,
			regexp.QuoteMeta(version),
		),
	)
	var prefixedFilepaths []string
	var globMatchedFilepaths []string

	for _, f := range files {
		if validFile.MatchString(f) {
			prefixedFilepaths = append(prefixedFilepaths, f)
		}
	}

	if len(prefixedFilepaths) == 0 {
		return nil, fmt.Errorf("no product files with expected prefix [%s,%s] found. Please ensure the file you're trying to download was initially persisted from Pivotal Network net using an appropriately configured download-product command", slug, version)
	}

	for _, f := range prefixedFilepaths {
		matched, _ := filepath.Match(glob, filepath.Base(f))
		if matched {
			globMatchedFilepaths = append(globMatchedFilepaths, f)
		}
	}

	if len(globMatchedFilepaths) > 1 {
		return nil, fmt.Errorf("the glob '%s' matches multiple files. Write your glob to match exactly one of the following:\n  %s", glob, strings.Join(globMatchedFilepaths, "\n  "))
	}

	if len(globMatchedFilepaths) == 0 {
		return nil, fmt.Errorf("the glob '%s' matches no file", glob)
	}

	return &FileArtifact{Name: globMatchedFilepaths[0]}, nil
}

func (s3 S3Client) DownloadProductToFile(fa *FileArtifact, destinationFile *os.File) error {
	blobReader, size, err := s3.initializeBlobReader(fa.Name)
	if err != nil {
		return err
	}

	progressBar, wrappedBlobReader := s3.startProgressBar(size, blobReader)
	defer progressBar.Finish()

	if err = s3.streamBufferToFile(destinationFile, wrappedBlobReader); err != nil {
		return err
	}

	return nil
}

func (s *S3Client) initializeBlobReader(filename string) (blobToRead io.ReadCloser, fileSize int64, err error) {
	location, err := s.stower.Dial("s3", s.Config)
	if err != nil {
		return nil, 0, err
	}
	container, err := location.Container(s.bucket)
	if err != nil {
		endpoint, _ := s.Config.Config("endpoint")
		if endpoint != "" {
			return nil, 0, errors.New(fmt.Sprintf(InvalidEndpointErrorMessageTemplate, endpoint, err.Error()))
		}
		return nil, 0, err
	}
	item, err := container.Item(filename)
	if err != nil {
		return nil, 0, err
	}

	fileSize, err = item.Size()
	if err != nil {
		return nil, 0, err
	}
	blobToRead, err = item.Open()
	return blobToRead, fileSize, err
}

func (s3 S3Client) startProgressBar(size int64, item io.Reader) (progressBar *progress.Bar, reader io.Reader) {
	progressBar = progress.NewBar()
	progressBar.SetTotal64(size)
	progressBar.SetOutput(s3.progressWriter)
	reader = progressBar.NewProxyReader(item)
	_, _ = s3.progressWriter.Write([]byte("Downloading product from s3..."))
	progressBar.Start()
	return progressBar, reader
}

func (s3 S3Client) streamBufferToFile(destinationFile *os.File, wrappedBlobReader io.Reader) error {
	_, err := io.Copy(destinationFile, wrappedBlobReader)
	return err
}

func (s3 S3Client) DownloadProductStemcell(fa *FileArtifact) (*stemcell, error) {
	return nil, errors.New("downloading stemcells for s3 is not supported at this time")
}

var InvalidEndpointErrorMessageTemplate = "Could not reach provided endpoint: '%s': %s"

func (s *S3Client) listFiles() ([]string, error) {
	location, err := s.stower.Dial("s3", s.Config)
	if err != nil {
		return nil, err
	}
	container, err := location.Container(s.bucket)
	if err != nil {
		endpoint, _ := s.Config.Config("endpoint")
		if endpoint != "" {
			return nil, errors.New(fmt.Sprintf(InvalidEndpointErrorMessageTemplate, endpoint, err.Error()))
		}
		return nil, err
	}

	var paths []string
	err = s.stower.Walk(container, stow.NoPrefix, 100, func(item stow.Item, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, item.ID())
		return nil
	})

	if err != nil {
		return nil, err
	}

	if len(paths) == 0 {
		return nil, fmt.Errorf("bucket contains no files")
	}

	return paths, nil
}

const Semver2Regex = `(?P<major>0|[1-9]\d*)\.(?P<minor>0|[1-9]\d*)\.(?P<patch>0|[1-9]\d*)(?:-(?P<prerelease>(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+(?P<buildmetadata>[0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?`
