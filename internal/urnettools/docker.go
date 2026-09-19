package urnettools

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// dockerContainer describes a discovered docker container running a
// URnetwork provider. The host-side tool manages container lifecycle and
// targeting; provider-internal operations are delegated to the container's
// own urnet-tools via docker exec.
type dockerContainer struct {
	// ID is the container ID (short form).
	ID string
	// Name is the container name (e.g. "urnet").
	Name string
	// Image is the image reference (e.g. "ghcr.io/3cape/urnetwork-provider:latest").
	Image string
	// State is docker's container state ("running", "exited", ...).
	State string
	// StateDir is the provider state dir INSIDE the container
	// (usually /root/.urnetwork, or $HOME/.urnetwork from env).
	StateDir string
}

// dockerCLI returns the docker binary name. Tests override by setting
// testDockerBin in a _test.go file with build tag urnettools_test_seams.
var testDockerBin string

func dockerCLI() string {
	if testDockerBin != "" {
		return testDockerBin
	}
	return "docker"
}

// discoverDockerContainers lists candidate containers via `docker ps -a`.
// A container is a candidate if its image name contains "urnetwork" or its
// name contains "urnet" (the fork image naming convention). We deliberately
// do NOT match generic names like "provider" to avoid false positives from
// unrelated software.
func discoverDockerContainers() []dockerContainer {
	cmd := exec.Command(dockerCLI(), "ps", "-a", "--no-trunc", "--format", "{{.ID}}|{{.Names}}|{{.Image}}|{{.State}}")
	out, err := cmd.Output()
	if err != nil {
		return nil // docker unavailable or no permission — no containers
	}
	var containers []dockerContainer
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		if len(parts) < 4 {
			continue
		}
		image := parts[2]
		name := parts[1]
		if !isDockerCandidate(image, name) {
			continue
		}
		containers = append(containers, dockerContainer{
			ID:    parts[0],
			Name:  name,
			Image: image,
			State: parts[3],
		})
	}
	return containers
}

// isDockerCandidate reports whether an image/name pair looks like a
// URnetwork provider container (including meso-miner images).
func isDockerCandidate(image, name string) bool {
	il := strings.ToLower(image)
	nl := strings.ToLower(name)
	if strings.Contains(il, "urnetwork") || strings.Contains(nl, "urnet") ||
		strings.Contains(il, "full-bars/sn") || strings.Contains(nl, "full-bars/sn") {
		return true
	}
	return strings.Contains(il, "meso-miner") || strings.Contains(il, "meso_miner") ||
		strings.Contains(nl, "meso-miner") || strings.Contains(nl, "meso_miner")
}

// containerStateDir resolves the provider state dir inside the container:
// prefer $HOME from the container's env, else /root (the image default).
func containerStateDir(c dockerContainer) string {
	if home := containerEnv(c, "HOME"); home != "" {
		return home + "/.urnetwork"
	}
	return "/root/.urnetwork"
}

// containerEnv reads one env var from the container via docker inspect.
func containerEnv(c dockerContainer, key string) string {
	cmd := exec.Command(dockerCLI(), "inspect", "-f", "{{range .Config.Env}}{{println .}}{{end}}", c.ID)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimPrefix(line, key+"=")
		}
	}
	return ""
}

// containerReadFile reads a file FROM a container. docker exec requires a
// RUNNING container, which silently dropped stopped providers from
// DiscoverDocker — making `urnet-docker start <stopped>` untargetable.
// docker cp works against stopped containers too (it reads the container's
// filesystem snapshot, no running process needed), so prefer it: copy to a
// temp file on the host and read that. `docker cp CONTAINER:path -` streams
// a TAR archive to stdout (not raw file content), which is why the stream
// is extracted with archive/tar rather than returned verbatim; falling back
// to docker exec keeps the RUNNING-container edge where cp disagrees.
func containerReadFile(c dockerContainer, path string) (string, error) {
	if out, err := exec.Command(dockerCLI(), "cp", c.ID+":"+path, "-").Output(); err == nil {
		if s, terr := extractSingleFileContent(out); terr == nil {
			return s, nil
		}
	}
	cmd := exec.Command(dockerCLI(), "exec", c.ID, "cat", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker cp/exec cat %s: %w (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// extractSingleFileContent pulls the content of the single regular file a
// `docker cp CONTAINER:path -` tar stream carries. Docker writes exactly one
// entry per file copy (plus a trailing directory entry for directory
// sources); anything else is a caller error.
func extractSingleFileContent(tarBytes []byte) (string, error) {
	r := bytes.NewReader(tarBytes)
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", fmt.Errorf("docker cp tar stream contained no file entry")
		}
		if err != nil {
			return "", fmt.Errorf("parse docker cp tar stream: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(io.LimitReader(tr, 1<<20)) // 1 MiB cap: identity files are tiny
			if err != nil {
				return "", fmt.Errorf("read %s from docker cp tar stream: %w", hdr.Name, err)
			}
			return string(data), nil
		}
		// Directory / other entry types: keep scanning for the regular file.
	}
}

// dockerProvider builds a host-facing Provider from a container by reading
// its JWT and version inside the container.
func dockerProvider(c dockerContainer) (Provider, error) {
	stateDir := containerStateDir(c)
	jwtRaw, err := containerReadFile(c, stateDir+"/jwt")
	if err != nil {
		return Provider{}, fmt.Errorf("container %s: read jwt: %w", c.Name, err)
	}
	// Write to a temp file so decodeJWT can parse it (it reads from disk).
	tmp, err := writeTempJWT(jwtRaw)
	if err != nil {
		return Provider{}, err
	}
	// The temp file holds a live network credential — always remove it,
	// success or failure.
	defer os.Remove(tmp)
	net, id, exp, err := decodeJWT(tmp)
	if err != nil {
		return Provider{}, fmt.Errorf("container %s: decode jwt: %w", c.Name, err)
	}
	p := Provider{
		User:       "docker:" + c.Name,
		StateDir:   stateDir,
		Binary:     "docker:" + c.Image,
		Unit:       c.Name,
		PID:        0,
		Running:    c.State == "running",
		Version:    dockerImageVersion(c.Image),
		Network:    net,
		NetworkID:  id,
		JWTExpires: exp,
	}
	return p, nil
}

// dockerImageVersion extracts a version-looking token from an image tag
// (e.g. "...:v3.23.0-fix.26.8" -> "v3.23.0-fix.26.8"). Best effort: only
// tags that look like release versions (v-prefixed or containing "fix")
// are returned; "latest"/"stable"/"nightly" yield "".
func dockerImageVersion(image string) string {
	i := strings.LastIndex(image, ":")
	if i < 0 {
		return ""
	}
	tag := image[i+1:]
	if strings.Contains(tag, "/") {
		return ""
	}
	switch tag {
	case "latest", "stable", "nightly", "dev":
		return ""
	}
	if !strings.HasPrefix(tag, "v") && !strings.Contains(tag, "fix") {
		return ""
	}
	return tag
}

// DiscoverDocker returns host-facing Provider records for every URnetwork
// container on the box, identified by their in-container JWT.
func DiscoverDocker() []Provider {
	containers := discoverDockerContainers()
	var out []Provider
	for _, c := range containers {
		if p, err := dockerProvider(c); err == nil {
			out = append(out, p)
		}
	}
	return out
}
