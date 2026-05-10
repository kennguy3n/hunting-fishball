//go:build integration

// Round-4 Task 13: contract tests for the Python gRPC services.
//
// The contract is asserted by walking each .proto file alongside
// its generated Go pb.go and verifying:
//
//  1. Every message + service name appears in both the .proto and
//     the generated Go source. (Catches the trivial "forgot to
//     re-run gen_protos.sh" footgun before it lands in CI.)
//  2. Every wire-protocol field number used in the .proto file
//     also appears in the Go-generated `protobuf:` struct tags.
//     This is the field-number contract — adding a field with a
//     new number is allowed, but renumbering an existing one
//     silently breaks every running client.
//  3. The Python-generated *_pb2.py declares the same message
//     names. We do this textually so the test does not need to
//     import the Python module.
package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	reMessage  = regexp.MustCompile(`(?m)^message\s+(\w+)\s*\{`)
	reService  = regexp.MustCompile(`(?m)^service\s+(\w+)\s*\{`)
	reFieldNum = regexp.MustCompile(`(?m)^\s+\w[\w\.]*\s+\w+\s*=\s*(\d+)\s*;`)
	reGoTagNum = regexp.MustCompile(`protobuf:"[^"]*?,(\d+),`)
)

// protoServices is the canonical list of (proto file, generated
// Go message file, generated Go gRPC file) tuples we contract-
// test. protoc-gen-go-grpc emits service stubs into a separate
// _grpc.pb.go file; the message structs live in the regular
// _pb.go.
var protoServices = []struct {
	proto string
	pbgo  string
	grpc  string
	pypb  string
}{
	{
		proto: "proto/docling/v1/docling.proto",
		pbgo:  "proto/docling/v1/docling.pb.go",
		grpc:  "proto/docling/v1/docling_grpc.pb.go",
		pypb:  "services/_proto/docling/v1/docling_pb2.py",
	},
	{
		proto: "proto/embedding/v1/embedding.proto",
		pbgo:  "proto/embedding/v1/embedding.pb.go",
		grpc:  "proto/embedding/v1/embedding_grpc.pb.go",
		pypb:  "services/_proto/embedding/v1/embedding_pb2.py",
	},
	{
		proto: "proto/memory/v1/memory.proto",
		pbgo:  "proto/memory/v1/memory.pb.go",
		grpc:  "proto/memory/v1/memory_grpc.pb.go",
		pypb:  "services/_proto/memory/v1/memory_pb2.py",
	},
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Tests run inside tests/integration; walk up to the repo
	// root by looking for go.mod.
	dir := wd
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("could not find repo root from %s", wd)
	return ""
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestProtoContract_MessagesPresentInGoStubs asserts that every
// message + service name in the .proto file is present in the
// generated Go pb.go. Catches stale generated stubs.
func TestProtoContract_MessagesPresentInGoStubs(t *testing.T) {
	root := repoRoot(t)
	for _, svc := range protoServices {
		t.Run(svc.proto, func(t *testing.T) {
			protoSrc := mustRead(t, filepath.Join(root, svc.proto))
			goSrc := mustRead(t, filepath.Join(root, svc.pbgo))

			messages := uniqueMatches(reMessage, protoSrc)
			for _, m := range messages {
				// Generated Go either declares `type <Msg> struct {` or
				// references it via `*<Msg>` in service stubs.
				want := "type " + m + " struct"
				if !strings.Contains(goSrc, want) {
					t.Errorf("Go stub missing %q (run `make proto-gen`)", want)
				}
			}
			services := uniqueMatches(reService, protoSrc)
			grpcSrc := mustRead(t, filepath.Join(root, svc.grpc))
			for _, s := range services {
				combined := goSrc + grpcSrc
				if !strings.Contains(combined, s+"Server") && !strings.Contains(combined, s+"Client") {
					t.Errorf("Go stub missing service interface %sServer/%sClient", s, s)
				}
			}
		})
	}
}

// TestProtoContract_FieldNumbersPreserved asserts every wire-
// protocol field number in the .proto appears in the generated
// Go's `protobuf:` tags. If a renumbering slips through, this
// test fails before deploy.
func TestProtoContract_FieldNumbersPreserved(t *testing.T) {
	root := repoRoot(t)
	for _, svc := range protoServices {
		t.Run(svc.proto, func(t *testing.T) {
			protoSrc := mustRead(t, filepath.Join(root, svc.proto))
			goSrc := mustRead(t, filepath.Join(root, svc.pbgo))

			protoNums := uniqueMatches(reFieldNum, protoSrc)
			goNums := uniqueMatches(reGoTagNum, goSrc)
			goSet := map[string]struct{}{}
			for _, n := range goNums {
				goSet[n] = struct{}{}
			}
			for _, n := range protoNums {
				if _, ok := goSet[n]; !ok {
					t.Errorf("field number %s present in .proto but missing from generated Go tags", n)
				}
			}
		})
	}
}

// TestProtoContract_PythonStubsExist asserts the Python pb2 file
// exists and references every message name. Skips with a
// diagnostic if the Python stubs have not been regenerated yet
// — that should ALSO fail CI but via a different target
// (`bash services/gen_protos.sh`).
func TestProtoContract_PythonStubsExist(t *testing.T) {
	root := repoRoot(t)
	for _, svc := range protoServices {
		t.Run(svc.proto, func(t *testing.T) {
			pyPath := filepath.Join(root, svc.pypb)
			pySrc, err := os.ReadFile(pyPath)
			if err != nil {
				t.Skipf("python stub %s not generated: %v", svc.pypb, err)
				return
			}
			protoSrc := mustRead(t, filepath.Join(root, svc.proto))
			messages := uniqueMatches(reMessage, protoSrc)
			py := string(pySrc)
			for _, m := range messages {
				if !strings.Contains(py, m) {
					t.Errorf("python stub missing message %s", m)
				}
			}
		})
	}
}

// TestProtoContract_BackwardCompat is a meta-check: asserts that
// no field number is reused for two different field names in the
// same proto file. The protoc compiler usually catches this, but
// we verify defensively because a hand-edited .proto can land
// without a regen step.
func TestProtoContract_BackwardCompat(t *testing.T) {
	root := repoRoot(t)
	for _, svc := range protoServices {
		t.Run(svc.proto, func(t *testing.T) {
			data := mustRead(t, filepath.Join(root, svc.proto))
			// Naive parse: walk message blocks and collect
			// (message, field, num) triples; flag duplicate (msg, num).
			lines := strings.Split(data, "\n")
			var msgStack []string
			used := map[string]map[int]string{}
			for _, ln := range lines {
				ln = strings.TrimSpace(ln)
				if m := reMessage.FindStringSubmatch(ln); m != nil {
					msgStack = append(msgStack, m[1])
					used[m[1]] = map[int]string{}
				}
				if strings.HasPrefix(ln, "}") && len(msgStack) > 0 {
					msgStack = msgStack[:len(msgStack)-1]
				}
				if len(msgStack) == 0 {
					continue
				}
				if m := reFieldNum.FindStringSubmatch(ln); m != nil {
					num := atoi(m[1])
					cur := msgStack[len(msgStack)-1]
					if prev, dup := used[cur][num]; dup && prev != "" {
						t.Errorf("%s.%s duplicates field number %d", cur, prev, num)
					}
					used[cur][num] = ln
				}
			}
			_ = fmt.Sprintf
			_ = sort.Slice
		})
	}
}

func uniqueMatches(re *regexp.Regexp, s string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		if _, ok := seen[m[1]]; ok {
			continue
		}
		seen[m[1]] = struct{}{}
		out = append(out, m[1])
	}
	return out
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
