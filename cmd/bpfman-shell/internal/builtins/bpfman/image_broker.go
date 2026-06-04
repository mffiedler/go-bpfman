package bpfmanbuiltin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/driver"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/runtime"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/internal/registryfixture"
)

const (
	e2eBytecodeSourceEnv     = "BPFMAN_E2E_BYTECODE_SOURCE"
	e2eBytecodeSourceImage   = "image"
	e2eRepoRootEnv           = "BPFMAN_E2E_REPO_ROOT"
	e2eImageBrokerPullPolicy = "Always"
)

var brokeredImageSeq uint64

var brokeredImageCache = struct {
	sync.Mutex
	refs map[string]string
}{refs: make(map[string]string)}

func maybeBrokerLoadFileArgs(ctx context.Context, args []runtime.Arg) ([]runtime.Arg, error) {
	if os.Getenv(e2eBytecodeSourceEnv) != e2eBytecodeSourceImage {
		return args, nil
	}
	if !isLoadFileArgs(args) {
		return args, nil
	}

	cmd, err := parseLoadFile(args[3:])
	if err != nil {
		return nil, err
	}
	ref, err := brokerBytecodeImage(ctx, cmd.Path)
	if err != nil {
		return nil, err
	}
	return loadImageArgsFromLoadFile(cmd, ref), nil
}

func isLoadFileArgs(args []runtime.Arg) bool {
	return len(args) >= 3 &&
		driver.ArgText(args[0]) == "program" &&
		driver.ArgText(args[1]) == "load" &&
		driver.ArgText(args[2]) == "file"
}

func loadImageArgsFromLoadFile(cmd *LoadFileCommand, imageRef string) []runtime.Arg {
	var argv []string
	argv = append(argv,
		"program", "load", "image",
		"--image-url", imageRef,
		"--pull-policy", e2eImageBrokerPullPolicy,
	)
	if len(cmd.Programs) > 0 {
		var specs []string
		for _, spec := range cmd.Programs {
			specs = append(specs, renderProgramSpec(spec))
		}
		argv = append(argv, "--programs", strings.Join(specs, ","))
	}
	for _, kv := range cmd.Metadata {
		argv = append(argv, "--metadata", kv.Key+"="+kv.Value)
	}
	for _, gd := range cmd.GlobalData {
		argv = append(argv, "--global", gd.Name+"=0x"+hex.EncodeToString(gd.Data))
	}
	if cmd.Application != "" {
		argv = append(argv, "--application", cmd.Application)
	}
	if cmd.MapOwnerID != 0 {
		argv = append(argv, "--map-owner-id", strconv.FormatUint(uint64(cmd.MapOwnerID), 10))
	}
	if cmd.Output.Output.IsSet {
		argv = append(argv, "-o", cmd.Output.Output.Value)
	}

	out := make([]runtime.Arg, 0, len(argv))
	for _, a := range argv {
		out = append(out, runtime.WordArg{Text: a})
	}
	return out
}

func renderProgramSpec(spec bpfmancli.ProgramSpec) string {
	parts := []string{spec.Type.String(), spec.Name}
	if spec.AttachFunc != "" {
		parts = append(parts, spec.AttachFunc)
	}
	return strings.Join(parts, ":")
}

func brokerBytecodeImage(ctx context.Context, bytecodePath string) (string, error) {
	registryHost, err := registryfixture.Host()
	if err != nil {
		return "", fmt.Errorf("%s=image requires registry host: %w", e2eBytecodeSourceEnv, err)
	}
	repoRoot := os.Getenv(e2eRepoRootEnv)
	if repoRoot == "" {
		return "", fmt.Errorf("%s=image requires %s", e2eBytecodeSourceEnv, e2eRepoRootEnv)
	}
	absBytecode, err := filepath.Abs(bytecodePath)
	if err != nil {
		return "", err
	}
	plan, err := planBrokeredBuild(repoRoot, absBytecode)
	if err != nil {
		return "", err
	}

	key := filepath.Clean(absBytecode)
	brokeredImageCache.Lock()
	if ref := brokeredImageCache.refs[key]; ref != "" {
		brokeredImageCache.Unlock()
		return ref, nil
	}
	ref := imageRefForBytecode(registryHost, plan.RelName)
	if err := buildBrokeredImage(ctx, plan.Bytecode, ref); err != nil {
		brokeredImageCache.Unlock()
		return "", err
	}
	brokeredImageCache.refs[key] = ref
	brokeredImageCache.Unlock()
	return ref, nil
}

// brokeredBuild names a brokered bytecode image and the object it is
// built from.
type brokeredBuild struct {
	RelName  string // repo-relative, slash-separated; names the image
	Bytecode string // path handed to `bpfman image build`
}

// planBrokeredBuild validates that absBytecode lies within repoRoot and
// returns the plan for building its image.
func planBrokeredBuild(repoRoot, absBytecode string) (brokeredBuild, error) {
	rel, err := filepath.Rel(repoRoot, absBytecode)
	if err != nil {
		return brokeredBuild{}, err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return brokeredBuild{}, fmt.Errorf("bytecode path %q is outside repository root %s", absBytecode, repoRoot)
	}
	return brokeredBuild{RelName: filepath.ToSlash(rel), Bytecode: absBytecode}, nil
}

func imageRefForBytecode(registryHost, relBytecode string) string {
	sum := sha256.Sum256([]byte(relBytecode))
	base := strings.TrimSuffix(filepath.Base(relBytecode), filepath.Ext(relBytecode))
	name := registryfixture.SanitiseComponent(base)
	tag := fmt.Sprintf("%x-%d-%d", sum[:6], os.Getpid(), atomic.AddUint64(&brokeredImageSeq, 1))
	return registryHost + "/" + registryfixture.RepositoryPrefix + "/" + name + ":" + tag
}

func buildBrokeredImage(ctx context.Context, bytecode, imageRef string) error {
	args := []string{
		"image", "build",
		imageRef,
		bytecode,
	}

	cmd, cancellationErr := newBPFManCommand(ctx, args...)
	out, err := cmd.CombinedOutput()
	if cancelErr := cancellationErr(); cancelErr != nil {
		return cancelErr
	}
	if err != nil {
		return fmt.Errorf("build bytecode image %s from %s: %w\n%s", imageRef, bytecode, err, strings.TrimSpace(string(out)))
	}
	return nil
}
