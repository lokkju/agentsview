package sshremote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"sync"

	"go.kenn.io/agentsview/internal/remotesync"
)

// zstdMagic starts every zstd frame. A response that starts otherwise was
// corrupted by output from the remote shell.
var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

const shellOutputMessage = "the remote shell printed output before agentsview ran; " +
	"make shell start-up files quiet for non-interactive sessions, as scp and rsync also require"

// shellOutputError carries its own summary because the cause is on the
// remote host and only the user can fix it.
type shellOutputError struct{}

func (shellOutputError) Error() string { return shellOutputMessage }

// FailureSummary is the message the CLI and API show for this failure.
func (shellOutputError) FailureSummary() string {
	return "SSH remote sync failed: " + shellOutputMessage
}

var errShellOutput error = shellOutputError{}

// Source fetches remote sync data by running agentsview over SSH.
type Source struct {
	provisioner Provisioner
	once        sync.Once
	binary      remoteBinary
	ensureErr   error
}

// NewSource returns a Source for one sync run.
func NewSource(p Provisioner) *Source { return &Source{provisioner: p} }

func (s *Source) ensure(ctx context.Context) (remoteBinary, error) {
	s.once.Do(func() { s.binary, s.ensureErr = s.provisioner.Ensure(ctx) })
	return s.binary, s.ensureErr
}

func serveScript(bin remoteBinary, op Op) (string, error) {
	if _, err := ParseOp(string(op)); err != nil {
		return "", err
	}
	script := "exec " + bin.shellPath + " remote-sync serve --op " + string(op)
	return script, validateScript(script)
}

func (s *Source) start(ctx context.Context, op Op, req serveRequest) (io.ReadCloser, error) {
	bin, err := s.ensure(ctx)
	if err != nil {
		return nil, err
	}
	script, err := serveScript(bin, op)
	if err != nil {
		return nil, err
	}
	req.ProtocolVersion = remotesync.ProtocolVersion
	var body bytes.Buffer
	if err := json.MarshalWrite(&body, req); err != nil {
		return nil, fmt.Errorf("encode %s request: %w", op, err)
	}
	stream, err := s.provisioner.Runner.Stream(ctx, script, &body)
	if err != nil {
		return nil, err
	}
	return newZstdChecked(stream), nil
}

func (s *Source) call(ctx context.Context, op Op, req serveRequest, out any) error {
	stream, err := s.start(ctx, op, req)
	if err != nil {
		return err
	}
	decodeErr := decodeZstdJSON(stream, out)
	closeErr := stream.Close()
	// A decode failure stops reading early, so Close then only reports that
	// it stopped ssh; the decode failure is the cause.
	if closeErr != nil && (decodeErr == nil || !errors.Is(closeErr, errStreamAbandoned)) {
		return closeErr
	}
	if decodeErr != nil {
		return fmt.Errorf("decode remote %s response: %w", op, decodeErr)
	}
	return nil
}

// Targets implements remotesync.Source.
func (s *Source) Targets(ctx context.Context) (remotesync.TargetSet, error) {
	var targets remotesync.TargetSet
	err := s.call(ctx, OpTargets, serveRequest{}, &targets)
	return targets, err
}

// Manifest implements remotesync.Source.
func (s *Source) Manifest(
	ctx context.Context, targets remotesync.TargetSet,
) (remotesync.Manifest, bool, error) {
	var manifest remotesync.Manifest
	err := s.call(ctx, OpManifest, serveRequest{Targets: &targets}, &manifest)
	if cmdErr, ok := errors.AsType[*CommandError](err); ok &&
		cmdErr.Remote != nil && cmdErr.Remote.Code == CodeUnsupported {
		return remotesync.Manifest{}, false, nil
	}
	return manifest, err == nil, err
}

// Archive implements remotesync.Source.
func (s *Source) Archive(
	ctx context.Context, request remotesync.ArchiveRequest,
) (*remotesync.ArchiveStream, error) {
	stream, err := s.start(ctx, OpArchive, serveRequest{Archive: &request})
	if err != nil {
		return nil, err
	}
	return &remotesync.ArchiveStream{Body: stream, Encoding: remotesync.ArchiveEncodingZstd}, nil
}

// zstdChecked fails the first Read when the stream does not start with a
// zstd frame. It only peeks, so the decoder still sees every byte.
type zstdChecked struct {
	r       *bufio.Reader
	closer  io.Closer
	checked bool
	err     error
}

func newZstdChecked(rc io.ReadCloser) io.ReadCloser {
	return &zstdChecked{r: bufio.NewReader(rc), closer: rc}
}

func (c *zstdChecked) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if !c.checked {
		c.checked = true
		// A stream shorter than the magic passes through: Read returns its
		// bytes and error, and an empty stream lets Close report the remote's
		// failure.
		head, _ := c.r.Peek(len(zstdMagic))
		if len(head) == len(zstdMagic) && !bytes.Equal(head, zstdMagic) {
			c.err = errShellOutput
			return 0, c.err
		}
	}
	return c.r.Read(p)
}

func (c *zstdChecked) Close() error { return c.closer.Close() }

// decodeZstdJSON decodes one zstd-compressed JSON value and reads r to EOF,
// so the ssh stream behind it ends normally.
func decodeZstdJSON(r io.Reader, out any) error {
	zr, err := remotesync.NewZstdReader(r)
	if err != nil {
		return err
	}
	defer zr.Close()
	if err := json.UnmarshalRead(zr, out); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return fmt.Errorf("read past response: %w", err)
	}
	return nil
}
