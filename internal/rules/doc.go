// Package rules implements the security rules language: the small
// declarative language an operator uses to say who may read and write which
// documents.
//
// The package is a classic three-stage interpreter:
//
//	source text --lexer--> tokens --parser--> syntax tree --evaluator--> decision
//
// It is part of the core of the hexagon. It performs no I/O of its own; when
// a rule needs another document (the get and exists functions) it calls a
// function supplied by the caller.
package rules
