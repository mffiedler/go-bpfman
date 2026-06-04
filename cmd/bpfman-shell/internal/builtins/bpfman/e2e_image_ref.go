package bpfmanbuiltin

import (
	"fmt"
	"strings"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/driver"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/runtime"
	"github.com/frobware/go-bpfman/internal/registryfixture"
)

func resolveE2EImageRef(ref string) (string, error) {
	if ref != registryfixture.RegistryAlias && !strings.HasPrefix(ref, registryfixture.RegistryAlias+"/") {
		return ref, nil
	}
	registryHost, err := registryfixture.Host()
	if err != nil {
		return "", err
	}
	suffix := strings.TrimPrefix(ref, registryfixture.RegistryAlias)
	suffix = strings.TrimPrefix(suffix, "/")
	if suffix == "" {
		return "", fmt.Errorf("%s reference requires an image name", registryfixture.RegistryAlias)
	}
	return strings.TrimSuffix(registryHost, "/") + "/" + suffix, nil
}

func resolveE2EImageRefsInArgs(args []runtime.Arg) ([]runtime.Arg, error) {
	out := make([]runtime.Arg, len(args))
	copy(out, args)
	if len(out) >= 3 &&
		driver.ArgText(out[0]) == "image" &&
		(driver.ArgText(out[1]) == "inspect" || driver.ArgText(out[1]) == "build") {
		resolved, err := resolveE2EImageRef(driver.ArgText(out[2]))
		if err != nil {
			return nil, err
		}
		out[2] = runtime.WordArg{Text: resolved}
	}
	for i := 0; i < len(out); i++ {
		text := driver.ArgText(out[i])
		switch text {
		case "--image-url", "-i", "--tag", "-t":
			if i+1 >= len(out) {
				continue
			}
			resolved, err := resolveE2EImageRef(driver.ArgText(out[i+1]))
			if err != nil {
				return nil, err
			}
			out[i+1] = runtime.WordArg{Text: resolved}
		default:
			for _, prefix := range []string{"--image-url=", "-i=", "--tag=", "-t="} {
				if strings.HasPrefix(text, prefix) {
					resolved, err := resolveE2EImageRef(strings.TrimPrefix(text, prefix))
					if err != nil {
						return nil, err
					}
					out[i] = runtime.WordArg{Text: prefix + resolved}
					break
				}
			}
		}
	}
	return out, nil
}
