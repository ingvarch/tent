// Package statestore keeps tent's state: each cluster's specs, completed spec, PKI, secrets, backups and history,
// as objects in a local directory (file://) or an S3-compatible bucket (s3://).
//
// Objects are addressed by relative paths of segments separated by single slashes, such as "prod/cluster.yaml".
// A segment holds only ASCII letters, digits, '_', '-' and '.', starts with a letter, digit or '_', does not end with
// '.', and is not a Windows device name (CON, PRN, AUX, NUL, COM1-9, LPT1-9, in any case and with any extension), so
// every path works in every backend on every operating system. Names starting with '.' are reserved for the
// backends' own files. List returns only valid paths.
//
// The file backend is for local disks only. Network file systems such as NFS emulate its file lock with per-process
// locks, which do not exclude goroutines of one process, so conditional puts would not be atomic there. It also
// differs from object stores in two ways. On a case-insensitive file system, the default on macOS and Windows, "a"
// and "A" are the same object. And a path cannot be both an object and a prefix of other objects, so "a" and "a/b"
// cannot both exist.
package statestore
