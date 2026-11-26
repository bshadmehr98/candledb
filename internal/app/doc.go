// Package app holds the use cases of the database: reading, querying,
// committing writes, listening for changes and managing accounts.
//
// It is the inner ring of the hexagon around the domain. Everything it needs
// from the outside world (storage, the clock, password hashing, tokens) it
// describes as a small interface in ports.go, and the adapters packages
// supply the implementations. app never imports an adapter.
package app
