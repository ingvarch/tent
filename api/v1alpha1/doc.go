// Package v1alpha1 holds the user-facing kinds of tent: Cluster and NodeGroup.
//
// It imports only the standard library, so any Go program can use the types.
package v1alpha1

//go:generate go run ../../internal/apischema/cmd/apischema -api . -out tent.schema.json
