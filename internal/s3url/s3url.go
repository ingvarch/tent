// Package s3url reads the URLs that name a bucket of an S3-compatible server, and a prefix in it, and makes clients
// for them. Credentials never go in a URL: clients take them from the standard AWS chain.
package s3url

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// URL is what an s3 URL says: s3://bucket[/prefix]?endpoint=https://host&region=name&pathStyle=true.
type URL struct {
	Bucket    string
	Prefix    string // without a slash at either end; empty for the whole bucket
	Endpoint  string // scheme://host of the server; empty for AWS
	Region    string // empty when the URL names none
	PathStyle bool   // for servers without bucket host names
}

// Parse reads raw, s3://bucket[/prefix]?endpoint=…&region=…&pathStyle=…. The endpoint is http or https and a host.
// checkPrefix, when not nil, checks a prefix that is not empty; Parse wraps its error. Errors never show the URL or a
// value of its query, which may hold a secret by mistake.
func Parse(raw string, checkPrefix func(prefix string) error) (URL, error) {
	// A slash in a password ends the authority, and the parser then sees no user: an @ anywhere is refused.
	if strings.Contains(raw, "@") {
		return URL{}, errors.New("remove the user and password: credentials come from the environment")
	}
	u, err := url.Parse(raw)
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err // without the URL
		}
		return URL{}, err
	}
	switch {
	case u.Scheme != "s3":
		return URL{}, errors.New("not an s3 URL: s3://bucket[/prefix]")
	case u.Host == "":
		return URL{}, errors.New("name a bucket: s3://bucket[/prefix]")
	case strings.Contains(u.Host, ":"):
		return URL{}, errors.New("a bucket has no port: name the server with endpoint=https://host:port")
	case u.Fragment != "":
		return URL{}, errors.New("an s3 URL takes no fragment")
	}
	p := URL{Bucket: u.Host, Prefix: strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/")}
	if p.Prefix != "" && checkPrefix != nil {
		if err := checkPrefix(p.Prefix); err != nil {
			return URL{}, fmt.Errorf("invalid prefix: %w", err)
		}
	}
	if err := p.readQuery(u.RawQuery); err != nil {
		return URL{}, err
	}
	return p, nil
}

// readQuery reads the endpoint, the region and pathStyle from the query of the URL.
func (p *URL) readQuery(rawQuery string) error {
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return err
	}
	for _, key := range slices.Sorted(maps.Keys(q)) {
		switch {
		case key != "endpoint" && key != "region" && key != "pathStyle":
			return errors.New("unknown query parameter: an s3 URL takes endpoint, region and pathStyle")
		case len(q[key]) > 1:
			return fmt.Errorf("%s is given more than once", key)
		}
	}
	if v, ok := q["region"]; ok {
		if p.Region = v[0]; p.Region == "" {
			return errors.New("region is empty")
		}
	}
	if v, ok := q["endpoint"]; ok {
		if p.Endpoint, err = parseEndpoint(v[0]); err != nil {
			return err
		}
	}
	if v, ok := q["pathStyle"]; ok {
		if p.PathStyle, err = strconv.ParseBool(v[0]); err != nil {
			return errors.New("pathStyle must be true or false")
		}
	}
	return nil
}

// parseEndpoint checks an endpoint, http or https and a host, and returns it as scheme://host.
func parseEndpoint(raw string) (string, error) {
	e, err := url.Parse(raw)
	if err == nil && e.User != nil {
		return "", errors.New("remove the user and password from the endpoint: credentials come from the environment")
	}
	if err != nil || (e.Scheme != "https" && e.Scheme != "http") || e.Host == "" || (e.Path != "" && e.Path != "/") ||
		e.RawQuery != "" || e.Fragment != "" {
		return "", errors.New("endpoint must be https:// or http:// and a host, without a path, query or fragment")
	}
	return e.Scheme + "://" + e.Host, nil
}

// String returns the URL without its region and path style, for messages: s3://bucket[/prefix][?endpoint=…].
func (p URL) String() string {
	s := "s3://" + p.Bucket
	if p.Prefix != "" {
		s += "/" + p.Prefix
	}
	if p.Endpoint != "" {
		s += "?endpoint=" + p.Endpoint
	}
	return s
}
