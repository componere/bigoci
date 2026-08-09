package mocks

import (
	"github.com/componere/bigoci/internal/oci"
)

// The generated mock must keep satisfying the port it doubles. This assert
// fails the build the moment the port changes without the mock being
// regenerated, because nothing else ties the two together at compile time.
var _ oci.Credentials = (*MockCredentials)(nil)
