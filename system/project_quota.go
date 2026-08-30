package system

import (
	"fmt"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ProjectQuota applies a filesystem-native, kernel-enforced size limit to one
// server data directory.
//
// It is intentionally separate from server/filesystem's accounting quota:
// that quota protects file-manager and SFTP operations performed by Omni,
// whereas ProjectQuota protects writes made directly by the server process
// inside the Docker bind mount. If this component accepts a limit, the kernel
// will return ENOSPC ("no space left on device") when the directory reaches it.
//
// ProjectQuota currently has drivers for ext4, XFS, Btrfs, and ZFS. These filesystems have
// incompatible management interfaces, which is why the driver is selected
// after detecting the filesystem that contains the server directory.
type ProjectQuota struct {
	// run executes a quota-management utility. It is injected for unit tests so
	// tests can validate the exact commands without needing root or a mounted
	// quota-enabled filesystem.
	run func(string, ...string) ([]byte, error)

	// detect is overridden by unit tests. Production uses filesystem below,
	// which reads the kernel mount table directly.
	detect func(string) (string, error)
}

// NewProjectQuota returns a quota manager that invokes host Linux utilities.
//
// The caller must run with sufficient privileges to manage filesystem quotas.
// Missing utilities or a filesystem that has not enabled project quotas are
// reported by Set as errors; they are never silently ignored.
func NewProjectQuota() *ProjectQuota {
	return &ProjectQuota{run: func(name string, args ...string) ([]byte, error) {
		// Quota utilities send actionable configuration errors to stderr. Keep
		// that output so the fallback warning tells an operator how to fix the
		// node instead of only reporting a generic exit status.
		return exec.Command(name, args...).CombinedOutput()
	}}
}

// Set assigns path to a project identified by serverID and sets its hard limit
// to limit bytes. A limit of zero disables the hard limit, matching the Panel's
// unlimited-disk setting.
//
// Set first reads /proc/self/mountinfo to determine the filesystem at path, then dispatches
// to the relevant driver. It supports ext4, XFS, Btrfs, and ZFS. An unsupported filesystem,
// disabled project quotas, or a failed host command all return an error. The
// caller decides whether to log it and retain the standard Omni limiter.
func (q *ProjectQuota) Set(path, serverID string, limit int64) error {
	if limit < 0 {
		return fmt.Errorf("project quota: negative disk limit for %q", serverID)
	}
	path = filepath.Clean(path)
	fs, err := q.filesystem(path)
	if err != nil {
		return err
	}
	// The project ID must be numeric. The UUID-derived CRC is deterministic, so
	// the same server receives the same project ID after an Omni restart.
	projectID := strconv.FormatUint(uint64(crc32.ChecksumIEEE([]byte(serverID)))+1, 10)

	switch fs {
	case "ext4":
		return q.setExt4(path, projectID, limit)
	case "xfs":
		return q.setXFS(path, projectID, limit)
	case "btrfs":
		return q.setBtrfs(path, limit)
	case "zfs":
		return q.setZFS(path, limit)
	default:
		return fmt.Errorf("project quota: filesystem %q at %q has no hard-quota driver", fs, path)
	}
}

func (q *ProjectQuota) filesystem(path string) (string, error) {
	if q.detect != nil {
		return q.detect(path)
	}
	// mountinfo is supplied by the Linux kernel. Reading it avoids depending on
	// findmnt from util-linux, which is often absent in minimal host images.
	// Select the longest matching mountpoint to handle a data directory located
	// below a nested mount instead of requiring system.data to be a mountpoint.
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", fmt.Errorf("project quota: read Linux mount information: %w", err)
	}

	var filesystem string
	longestMountpoint := -1
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			continue
		}
		left, right := strings.Fields(parts[0]), strings.Fields(parts[1])
		// The mountpoint is field 5 before the separator; field 1 after it is
		// the filesystem type. See proc_pid_mountinfo(5).
		if len(left) < 5 || len(right) < 1 {
			continue
		}
		mountpoint := unescapeMountinfoPath(left[4])
		if !isPathInMount(path, mountpoint) {
			continue
		}
		if len(mountpoint) > longestMountpoint {
			filesystem, longestMountpoint = right[0], len(mountpoint)
		}
	}
	if filesystem == "" {
		return "", fmt.Errorf("project quota: no mount found for %q", path)
	}
	return filesystem, nil
}

// isPathInMount reports whether path is the mountpoint or one of its children.
// The root mount is special: appending a slash to it would produce "//".
func isPathInMount(path, mountpoint string) bool {
	return mountpoint == "/" || path == mountpoint || strings.HasPrefix(path, mountpoint+"/")
}

// unescapeMountinfoPath decodes the octal escape sequences used by mountinfo
// for spaces, tabs, newlines, and backslashes in mount paths.
func unescapeMountinfoPath(path string) string {
	for _, replacement := range []struct{ old, new string }{
		{`\040`, " "}, {`\011`, "\t"}, {`\012`, "\n"}, {`\134`, `\`},
	} {
		path = strings.ReplaceAll(path, replacement.old, replacement.new)
	}
	return path
}

func (q *ProjectQuota) setExt4(path, projectID string, limit int64) error {
	// +P causes new descendants to inherit the directory's project ID; -p sets
	// that ID on the existing root directory. Both are essential: only setting
	// the root leaves newly-created files outside the project's accounting.
	if _, err := q.run("chattr", "+P", "-p", projectID, path); err != nil {
		return fmt.Errorf("project quota: assign ext4 project to %q: %w", path, err)
	}
	// setquota uses KiB blocks. Panel limits are MiB, so the conversion is exact
	// and does not round the customer allocation upward.
	hardLimit := strconv.FormatInt(limit/1024, 10) + "K"
	if _, err := q.run("setquota", "-P", projectID, "0", hardLimit, "0", "0", path); err != nil {
		return fmt.Errorf("project quota: set ext4 quota for %q: %w", path, err)
	}
	return nil
}

func (q *ProjectQuota) setXFS(path, projectID string, limit int64) error {
	// xfs_quota accepts management commands through -c. The project command
	// associates the directory with its ID; bhard is the non-negotiable byte
	// limit, unlike bsoft which would permit temporary overages.
	project := "project -s -p " + shellQuote(path) + " " + projectID
	hardLimit := "limit -p bhard=" + strconv.FormatInt(limit, 10) + "b " + projectID
	// xfs_quota requires a trailing mountpoint/path to select the target XFS
	// filesystem. Without it, the commands have no current filesystem and the
	// utility exits with status 1 before it can configure the project.
	output, err := q.run("xfs_quota", "-x", "-c", project, "-c", hardLimit, path)
	if err != nil {
		return commandError("set XFS quota", path, err, output)
	}
	return nil
}

// commandError preserves an external tool's stderr/stdout in the returned
// error. Utilities such as xfs_quota print missing-prjquota and permission
// diagnostics there, which is essential when Omni chooses the safe fallback.
func commandError(action, path string, err error, output []byte) error {
	message := strings.TrimSpace(string(output))
	if message == "" {
		return fmt.Errorf("project quota: %s for %q: %w", action, path, err)
	}
	return fmt.Errorf("project quota: %s for %q: %w: %s", action, path, err, message)
}

// setBtrfs limits the qgroup associated with the server subvolume. Btrfs
// qgroups exist only for subvolumes, not ordinary directories, so a node using
// this driver must create each server root as a Btrfs subvolume and enable
// quotas once for the parent filesystem (`btrfs quota enable <mountpoint>`).
//
// We do not create a subvolume here: converting an existing directory could
// hide or orphan server data. A non-subvolume is reported as an error and Omni
// safely continues using its standard application-level disk limiter.
func (q *ProjectQuota) setBtrfs(path string, limit int64) error {
	hardLimit := "none"
	if limit > 0 {
		hardLimit = strconv.FormatInt(limit, 10)
	}
	if _, err := q.run("btrfs", "qgroup", "limit", hardLimit, path); err != nil {
		return fmt.Errorf("project quota: set Btrfs qgroup limit for %q: %w", path, err)
	}
	return nil
}

// setZFS applies refquota to the dataset mounted at path. refquota is used
// rather than quota because it limits the dataset's own referenced data and
// does not charge space used by child datasets to the server allocation.
//
// ZFS has quotas per dataset, not per arbitrary directory. The server root
// must therefore be the mountpoint of a dedicated ZFS dataset. We locate the
// longest dataset mountpoint matching path and reject a parent dataset to avoid
// accidentally applying one server's limit to every server stored beneath it.
func (q *ProjectQuota) setZFS(path string, limit int64) error {
	out, err := q.run("zfs", "list", "-H", "-p", "-o", "name,mountpoint", "-t", "filesystem")
	if err != nil {
		return fmt.Errorf("project quota: list ZFS datasets: %w", err)
	}

	var dataset string
	longestMountpoint := -1
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !isPathInMount(path, fields[1]) {
			continue
		}
		if len(fields[1]) > longestMountpoint {
			dataset, longestMountpoint = fields[0], len(fields[1])
		}
	}
	if dataset == "" || filepath.Clean(path) != filepath.Clean(strings.TrimSpace(datasetMountpoint(out, dataset))) {
		return fmt.Errorf("project quota: %q is not the mountpoint of a dedicated ZFS dataset", path)
	}

	hardLimit := "none"
	if limit > 0 {
		hardLimit = strconv.FormatInt(limit, 10)
	}
	if _, err := q.run("zfs", "set", "refquota="+hardLimit, dataset); err != nil {
		return fmt.Errorf("project quota: set ZFS refquota for %q: %w", path, err)
	}
	return nil
}

// datasetMountpoint returns the mountpoint listed for dataset by zfs list.
// Keeping parsing in one place makes setZFS's dedicated-dataset check explicit.
func datasetMountpoint(output []byte, dataset string) string {
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == dataset {
			return fields[1]
		}
	}
	return ""
}

func shellQuote(s string) string {
	// xfs_quota parses the command string itself. Quote the path defensively so
	// a configured data directory containing spaces or quotes stays one value.
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
