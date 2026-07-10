package image

// Dockerfile exposes the unexported dockerfile() composer to tests in the
// external test package so the composed layout (base, then Dockerfile.extra,
// then ccc's verification footer) can be asserted directly.
func (b *Builder) Dockerfile() ([]byte, error) { return b.dockerfile() }

// ContentHashFor exposes the unexported contentHashFor so a test can recompute
// the tag from the exact Dockerfile bytes written into the build context and
// assert that the built image corresponds to the tag it was built under.
func (b *Builder) ContentHashFor(df []byte) string { return b.contentHashFor(df) }
