package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ingvarch/tent/internal/s3url"
)

// urlEnv names the environment variable that holds the bucket URL.
const urlEnv = "TENT_DEV_S3_URL"

// urlForm is the form of the bucket URL, as Cloudflare R2 takes it.
const urlForm = "s3://bucket/prefix?endpoint=https://host&region=auto"

// segmentPattern matches a segment of a prefix: ASCII letters, digits, '_', '-' and '.', starting with a letter, a
// digit or '_'.
var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// parseBucketURL reads the value of urlEnv, an s3 URL as tent's state store takes it. The prefix is required: the
// bucket's lifecycle rule deletes what is below it. Its errors never show the URL or a value of its query.
func parseBucketURL(raw string) (s3url.URL, error) {
	if raw == "" {
		return s3url.URL{}, errors.New(urlEnv + " is not set: set it to " + urlForm)
	}
	u, err := s3url.Parse(raw, checkPrefix)
	if err == nil && u.Prefix == "" {
		err = errors.New("name a prefix, such as s3://bucket/dev: the bucket's lifecycle rule deletes what is below it")
	}
	if err != nil {
		return s3url.URL{}, fmt.Errorf("%s: %w", urlEnv, err)
	}
	return u, nil
}

// checkPrefix checks the segments of a prefix against segmentPattern.
func checkPrefix(prefix string) error {
	for seg := range strings.SplitSeq(prefix, "/") {
		if !segmentPattern.MatchString(seg) {
			return fmt.Errorf("segment %q: use only ASCII letters, digits, '_', '-' and '.', and start with a letter, "+
				"digit or '_'", seg)
		}
	}
	return nil
}

// bucket is the bucket that the tool uploads to.
type bucket struct {
	client *s3.Client
	name   string
}

// openBucket returns the bucket that loc names, reached with the credentials of the standard AWS chain. It sends
// nothing.
func openBucket(ctx context.Context, loc s3url.URL) (*bucket, error) {
	client, err := loc.Client(ctx)
	if errors.Is(err, s3url.ErrNoRegion) {
		return nil, fmt.Errorf("%s: %w", urlEnv, err)
	}
	if err != nil {
		return nil, err
	}
	return &bucket{client: client, name: loc.Bucket}, nil
}

// credentials returns the provider of the credentials that the bucket's client signs with.
func (b *bucket) credentials() aws.CredentialsProvider { return b.client.Options().Credentials }

// credentialsWarning gets the credentials of p and returns a warning when they may stop working before until, and
// the URL with them: temporary credentials, such as a session's or SSO's, expire. Static keys, such as an R2 token's,
// get none. The warning shows no part of the credentials.
func credentialsWarning(ctx context.Context, p aws.CredentialsProvider, until time.Time) (string, error) {
	c, err := p.Retrieve(ctx)
	if err != nil {
		return "", fmt.Errorf("get the AWS credentials: %w", err)
	}
	switch {
	case c.CanExpire && c.Expires.Before(until):
		return fmt.Sprintf("the AWS credentials expire at %s, before the URL does: the URL stops working then",
			c.Expires.UTC().Format(time.RFC3339)), nil
	case !c.CanExpire && c.SessionToken != "":
		return "the AWS credentials carry a session token: the URL stops working when the session ends, which may " +
			"be before the URL expires", nil
	}
	return "", nil
}

// uploaded returns when the object at key was uploaded, and false when the bucket holds none. The time is zero when
// the server does not say.
func (b *bucket) uploaded(ctx context.Context, key string) (time.Time, bool, error) {
	out, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(b.name), Key: aws.String(key)})
	if err == nil {
		return aws.ToTime(out.LastModified), true, nil
	}
	if e, ok := errors.AsType[interface {
		error
		HTTPStatusCode() int
	}](err); ok && e.HTTPStatusCode() == http.StatusNotFound {
		return time.Time{}, false, nil
	}
	return time.Time{}, false, err
}

// put uploads size bytes from body to key.
func (b *bucket) put(ctx context.Context, key string, body io.ReadSeeker, size int64) error {
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(b.name), Key: aws.String(key), Body: body, ContentLength: aws.Int64(size),
		ContentType: aws.String("application/octet-stream"),
	})
	return err
}

// presign returns a URL that downloads the object at key with a plain GET until expires has passed.
func (b *bucket) presign(ctx context.Context, key string, expires time.Duration) (string, error) {
	req, err := s3.NewPresignClient(b.client).PresignGetObject(ctx,
		&s3.GetObjectInput{Bucket: aws.String(b.name), Key: aws.String(key)}, s3.WithPresignExpires(expires))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}
