package statestore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// schemes says which stores Open takes.
const schemes = "use file:///abs/path for a local directory or s3://bucket[/prefix] for a bucket"

// Option changes how OpenWith opens a store.
type Option func(*options)

type options struct {
	noSync bool
}

// WithoutSync makes a file store skip the syncs that make a write survive a power loss: of the file and of its
// directory. Everything else stays: the atomic rename, the modes, the lock and the errors. A bucket ignores it. It is
// for tests, whose stores need not survive a power loss; tent itself never passes it.
func WithoutSync() Option { return func(o *options) { o.noSync = true } }

// Open opens the store at a URL. Credentials never go in the URL.
//
//   - file:///abs/path is a local directory (file:///C:/state on Windows).
//   - s3://bucket[/prefix]?endpoint=https://host&region=name&pathStyle=true is an S3-compatible bucket. The endpoint
//     is needed except on AWS, the region unless the AWS configuration names one (Cloudflare R2 takes auto), and
//     pathStyle=true only for servers without bucket host names. Credentials come from the standard AWS chain: the
//     environment, the shared files, or the role of the machine.
func Open(ctx context.Context, rawURL string) (Store, error) {
	return OpenWith(ctx, rawURL)
}

// OpenWith is Open with options. The options are for tests; production code calls Open.
func OpenWith(ctx context.Context, rawURL string, opts ...Option) (Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("open state store: %w", err)
	}
	// Errors never show the URL: it may hold a secret. An @ is checked before parsing because a slash in a password
	// ends the authority, and the parser then does not see a user at all.
	if strings.Contains(rawURL, "@") {
		return nil, errors.New("state store URL: remove the user and password: credentials come from the " +
			"environment (write an @ in a path as %40)")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err // without the URL
		}
		return nil, fmt.Errorf("state store URL: %w", err)
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	var backend Store
	switch u.Scheme {
	case "file":
		backend, err = newFileStore(u, o)
	case "s3":
		backend, err = newS3Store(ctx, rawURL)
	case "":
		return nil, errors.New("state store URL has no scheme: " + schemes)
	default:
		return nil, fmt.Errorf("state store URL: unsupported scheme %q: %s", u.Scheme, schemes)
	}
	if err != nil {
		return nil, err
	}
	return checkedStore{backend}, nil
}
