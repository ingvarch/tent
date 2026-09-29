package s3url

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/logging"
)

// ErrNoRegion is the error of Client for a URL without a region when the AWS configuration names none either.
var ErrNoRegion = errors.New("no region: add region=… (Cloudflare R2 takes auto) or set AWS_REGION")

// Client returns a client of the bucket's server, with the credentials of the standard AWS chain: the environment, the
// shared files or the role of the machine. The URL's region wins over the configuration's. The client asks for
// checksums only where the API requires them: some servers, such as Ceph RGW, send none with a GET, and a presigned
// download stays a plain GET. Client sends nothing.
func (p URL) Client(ctx context.Context) (*s3.Client, error) {
	// The SDK logs to stderr by default.
	opts := []func(*config.LoadOptions) error{config.WithLogger(logging.Nop{})}
	if p.Region != "" {
		opts = append(opts, config.WithRegion(p.Region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load the AWS configuration: %w", err)
	}
	if cfg.Region == "" {
		return nil, ErrNoRegion
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		if p.Endpoint != "" {
			o.BaseEndpoint = aws.String(p.Endpoint)
		}
		o.UsePathStyle = p.PathStyle
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	}), nil
}
