package image

// Dockerfile exposes the unexported dockerfile() composer to tests in the
// external test package so the composed layout (base, then Dockerfile.extra,
// then ccc's verification footer) can be asserted directly.
func (b *Builder) Dockerfile() ([]byte, error) { return b.dockerfile() }
