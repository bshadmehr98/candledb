// Package domain is the centre of the hexagon: the vocabulary of the
// database (values, documents, paths, queries, writes) and the pure rules
// that govern them.
//
// Nothing in this package performs I/O and it imports only the standard
// library, so every function here can be tested with plain values.
package domain
