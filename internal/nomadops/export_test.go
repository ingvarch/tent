package nomadops

import "time"

// SetTimeout sets how long each call of the client may take, for tests.
func (c *Client) SetTimeout(d time.Duration) { c.timeout = d }

// NewCallError is the error of a call that failed without an answer's status, for tests.
var NewCallError = newCallError
