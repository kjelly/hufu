package main

import (
	"context"
	"io"
)

// Run echoes an empty structured result; the example only needs to load.
func Run(ctx context.Context, in io.Reader, out io.Writer) error {
	if _, err := io.Copy(io.Discard, in); err != nil {
		return err
	}
	_, err := io.WriteString(out, `{"outputs":{},"artifacts":[]}`)
	return err
}
