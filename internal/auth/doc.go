// Package auth resolves the credential bigoci presents to a registry.
//
// It implements the oci package's Credentials port twice. [Store] reads the
// Docker configuration file that `docker login` writes, which is where a
// credential usually already is, and answers with what it finds under the
// host bigoci dialed. [Static] answers every registry with one credential the
// caller already holds, which is what a CI job with a token in its
// environment has.
//
// This is the only place oras-go enters bigoci's import graph, and it borrows
// exactly one thing from it: the credential store. The bearer exchange, the
// token cache, and the decision of what a refusal is worth all live in the
// oci package, because they are transfer behaviour rather than credential
// storage.
//
// Reading a credential can run a program on the machine. A Docker
// configuration file may name a credential helper, in its credsStore or
// credHelpers field, and a [Store] lookup then executes the program called
// docker-credential-<name> from the process PATH and reads the credential off
// what it prints. Which program that is, and where it comes from, is the
// user's configuration and not bigoci's choice. It is what a tool that honours
// `docker login` has to do, and it is why the public option that builds a
// Store is opt-in: a caller that never asks for it reads no file and runs no
// program. A lookup is given [credLookupTimeout] to finish, so a helper that
// never answers fails a transfer instead of hanging it.
//
// Nothing here can write to a user's configuration. The port has one read
// method, so the store's Put and Delete are unreachable through it and a
// credential-writing bug is unrepresentable rather than merely absent.
package auth
