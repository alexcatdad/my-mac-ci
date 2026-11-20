package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Zot permits anonymous reads. Push authentication stays with Apple container.
func manifest(ctx context.Context, registry, name, ref string) (string, error) {
	host, namespace, _ := strings.Cut(registry, "/")
	if namespace != "" {
		namespace += "/"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/v2/"+namespace+name+"/manifests/"+ref, nil)
	if err != nil {
		return "", errors.New("invalid manifest request")
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	client := http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("cannot reach image registry")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry manifest lookup returned HTTP %d", resp.StatusCode)
	}
	h := sha256.New()
	const limit = 16 << 20
	n, err := io.Copy(h, io.LimitReader(resp.Body, limit+1))
	if err != nil || n == 0 || n > limit {
		return "", errors.New("invalid registry manifest")
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}

func publish(ctx context.Context, registry, name, tag, local, digest string) (string, error) {
	remote, err := manifest(ctx, registry, name, tag)
	if err != nil {
		return "", err
	}
	if remote != "" && remote != digest {
		return "", errors.New("content tag already refers to a different image")
	}
	if remote == "" {
		target := registry + "/" + name + ":" + tag
		if err = command(ctx, time.Minute, "", nil, "container", "image", "tag", local, target); err != nil {
			return "", errors.New("could not tag the built image")
		}
		if err = command(ctx, 10*time.Minute, "", nil, "container", "image", "push", target); err != nil {
			return "", errors.New("could not push image with the local registry credentials")
		}
		remote, err = manifest(ctx, registry, name, tag)
		if err != nil {
			return "", err
		}
	}
	if remote != digest {
		return "", errors.New("published tag failed digest verification")
	}
	byDigest, err := manifest(ctx, registry, name, digest)
	if err != nil {
		return "", err
	}
	if byDigest != digest {
		return "", errors.New("published digest failed verification")
	}
	ref := name + "@" + digest
	if err := command(ctx, 10*time.Minute, "", nil, "container", "image", "pull", registry+"/"+ref); err != nil {
		return "", errors.New("image published but local digest pull failed")
	}
	return ref, nil
}
