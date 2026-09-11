package asr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// defaultFakeSRT is a minimal but syntactically valid srt fixture that
// FakeRecognizer writes when the caller has not injected [FakeRecognizer.Payload]
// or [FakeRecognizer.FixturePath]. It is deliberately embedded (rather than
// read from testdata/sample-asr.srt) so [FakeRecognizer] stays useful in
// downstream packages whose working directory does not sit next to the
// fixtures tree — e.g. pipeline unit tests running from
// internal/pipeline/. The two-cue content mirrors
// [testdata/sample-asr.srt] so pipeline assertions can match against
// either source interchangeably.
const defaultFakeSRT = "1\n" +
	"00:00:00,000 --> 00:00:03,500\n" +
	"大家好 欢迎收看本期视频 今天我们\n" +
	"\n" +
	"2\n" +
	"00:00:03,500 --> 00:00:07,200\n" +
	"来聊一聊字幕自动化 这段是ASR识别出的文本\n"

// FakeRecognizer is the unit-test [Recognizer]. It writes an srt file
// at `<outPrefix>.srt` (matching the "produces an srt file" post-
// condition) without spawning whisper-cli. Downstream code that only
// cares about the file's existence / raw bytes — e.g. pipeline
// plumbing that hands the srt path to a subtitle.Parser — is fully
// exercisable through this.
//
// The zero value works: no error injection, uses [defaultFakeSRT] as
// payload, honours the caller's ctx. All fields exist purely to steer
// tests:
//
//   - Err: return this error verbatim (after checking ctx.Err()) and
//     do not touch the filesystem. Emulates "whisper-cli exited
//     non-zero".
//   - Payload: raw bytes to write at `<outPrefix>.srt` instead of
//     [defaultFakeSRT]. Takes precedence over [FixturePath] when both
//     are set — Payload is the "explicit override", FixturePath is
//     the "load from disk" convenience.
//   - FixturePath: absolute or CWD-relative path to a srt file whose
//     contents are copied verbatim to `<outPrefix>.srt`. Useful for
//     integration-style tests that want to exercise a specific
//     fixture (e.g. [testdata/sample-asr.srt]) without embedding it
//     into every test file.
//   - Calls: append (wav, model, prefix) for each invocation so tests
//     can assert "was called exactly N times with these paths"
//     without wrapping the fake in another mock.
type FakeRecognizer struct {
	Err         error
	Payload     []byte
	FixturePath string
	Calls       []FakeCall
}

// FakeCall records a single Transcribe invocation. All fields are the
// values the caller actually passed — no path normalisation is applied
// so tests see exactly what the pipeline handed us.
type FakeCall struct {
	Wav    string
	Model  string
	Prefix string
}

// Transcribe implements [Recognizer] for tests.
//
// Semantics — deliberately narrower than the real [WhisperRecognizer]
// so tests are easy to reason about:
//
//   - ctx cancellation is honoured *before* any filesystem work: if
//     ctx.Done() has already fired we return ctx.Err() wrapped in the
//     same "asr: fake: %w" envelope the real implementation uses for
//     ctx errors. This keeps `errors.Is(err, context.Canceled)`
//     assertions portable between fake and real.
//   - Empty wav/model/prefix are rejected up front, mirroring the
//     real implementation so pipeline code that violates the contract
//     fails identically against both.
//   - `outPrefix` ending in `.srt` is rejected (both fake and real
//     reject it) so callers don't accidentally produce `<name>.srt.srt`.
//   - Err (when non-nil) is returned verbatim without touching disk —
//     no partial `.srt` is left behind, matching the "failed run must
//     not produce output" convention the fakebin scripts also follow.
//     The empty srtPath return keeps callers from accidentally
//     stat-ing a non-existent file after an error.
//   - On success, [os.WriteFile] truncates + writes
//     `<outPrefix>.srt` (matching whisper-cli's overwrite behaviour).
//     Precedence for the payload bytes is:
//     (1) `len(Payload) > 0` — explicit non-empty override, wins over
//     everything;
//     (2) `FixturePath != ""` — read from disk at call time;
//     (3) `defaultFakeSRT` — embedded default.
//     An **empty** Payload (`len == 0` but non-nil) is treated as
//     "no payload set" and falls through to FixturePath/default so
//     tests don't accidentally emit an empty srt that trips
//     subtitle.ErrEmpty downstream. Tests that specifically want an
//     empty srt should use `FixturePath` pointing at an empty file.
//   - The parent directory of outPrefix MUST exist (contract
//     clause) — os.WriteFile surfaces the underlying ENOENT
//     untouched.
//   - The returned srtPath is normalised via [filepath.Abs] so
//     callers get the absolute form the contract mandates, regardless
//     of whether the caller passed an absolute or CWD-relative prefix.
func (f *FakeRecognizer) Transcribe(ctx context.Context, wavPath, modelPath, outPrefix string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("asr: fake: %w", err)
	}
	wavPath = strings.TrimSpace(wavPath)
	modelPath = strings.TrimSpace(modelPath)
	outPrefix = strings.TrimSpace(outPrefix)
	if wavPath == "" {
		return "", errors.New("asr: fake: empty wav path")
	}
	if modelPath == "" {
		return "", errors.New("asr: fake: empty model path")
	}
	if outPrefix == "" {
		return "", errors.New("asr: fake: empty output prefix")
	}
	if strings.HasSuffix(strings.ToLower(outPrefix), ".srt") {
		return "", fmt.Errorf("asr: fake: outPrefix %q must not end with .srt", outPrefix)
	}

	// Record BEFORE injected error so tests can still verify the call
	// happened when Err is set.
	f.Calls = append(f.Calls, FakeCall{Wav: wavPath, Model: modelPath, Prefix: outPrefix})

	if f.Err != nil {
		return "", f.Err
	}

	// Resolve payload precedence: explicit non-empty bytes > fixture
	// file > embedded default. FixturePath is read at call time so
	// callers can point at fixtures that are generated dynamically by
	// the test (e.g. under t.TempDir()).
	var payload []byte
	switch {
	case len(f.Payload) > 0:
		payload = f.Payload
	case f.FixturePath != "":
		b, err := os.ReadFile(f.FixturePath)
		if err != nil {
			return "", fmt.Errorf("asr: fake: read fixture %s: %w", f.FixturePath, err)
		}
		payload = b
	default:
		payload = []byte(defaultFakeSRT)
	}

	srtPath := outPrefix + ".srt"
	if err := os.WriteFile(srtPath, payload, 0o644); err != nil {
		return "", fmt.Errorf("asr: fake: write %s: %w", srtPath, err)
	}
	abs, err := filepath.Abs(srtPath)
	if err != nil {
		return "", fmt.Errorf("asr: fake: abs(%s): %w", srtPath, err)
	}
	return abs, nil
}

// Compile-time check: FakeRecognizer satisfies [Recognizer].
var _ Recognizer = (*FakeRecognizer)(nil)
