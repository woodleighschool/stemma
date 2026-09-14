package apple

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

type cancelingReader struct {
	*bytes.Reader

	cancel context.CancelFunc
	reads  int
}

func (r *cancelingReader) Read(p []byte) (int, error) {
	r.reads++
	n, err := r.Reader.Read(p)
	r.cancel()
	return n, err
}

func TestFileDigestStopsBetweenReads(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	input := &cancelingReader{Reader: bytes.NewReader(make([]byte, 1<<20)), cancel: cancel}
	if _, err := fileDigest(ctx, input, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("digest: %v", err)
	}
	if input.reads != 1 {
		t.Fatalf("continued reading after cancellation: %d reads", input.reads)
	}
}

func TestCanceledVerificationDoesNotReadInput(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := VerifyApp(ctx, "missing.app", Policy{RequireIntegrity: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("app: %v", err)
	}
	if _, err := VerifyPackage(ctx, "missing.pkg", Policy{RequireIntegrity: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pkg: %v", err)
	}
}

type cancelingReaderAt struct {
	reader io.ReaderAt
	cancel context.CancelFunc
	reads  int
}

func (r *cancelingReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	r.reads++
	n, err := r.reader.ReadAt(p, offset)
	r.cancel()
	return n, err
}

func TestExecutableVerificationStopsBetweenPages(t *testing.T) {
	data := readTestFile(t, "testdata/SignedFixture.app/Contents/MacOS/fixture")
	slices, err := machoSlices(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	slice := slices[0]
	signature, err := slice.signature()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	input := &cancelingReaderAt{reader: slice.r, cancel: cancel}
	slice.r = contextReaderAt{ctx, input}
	if err := slice.verifyCodeDirectory(signature.directories[0], signature, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("verification: %v", err)
	}
	if input.reads != 1 {
		t.Fatalf("continued reading after cancellation: %d reads", input.reads)
	}
}
