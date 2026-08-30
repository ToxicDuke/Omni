# Kernel-enforced disk limits

Omni can make the Panel disk limit a hard limit enforced by Linux. Omni detects
the filesystem that contains `system.data` and uses its native project-quota
driver; Docker then
bind-mounts that directory into `/home/container`, so every write from the
server process is accounted to the same quota.

Prepare the host filesystem before enabling the feature:

1. For **ext4**, enable the project-quota filesystem feature and mount it with
   `prjquota`; install `e2fsprogs` and `quota` (`chattr`, `setquota`).
2. For **XFS**, mount with `prjquota` (or `pquota`) and install `xfsprogs`
   (`xfs_quota`).
3. For **Btrfs**, make each server data root a Btrfs subvolume, enable qgroups
   once on the filesystem (`btrfs quota enable <mountpoint>`), and install
   `btrfs-progs`. Omni intentionally will not convert an existing directory
   into a subvolume because that could hide existing server data.
4. For **ZFS**, create and mount a dedicated dataset at each server data root.
   Omni applies `refquota` to that dataset; it does not apply a quota to a
   parent dataset containing multiple servers. Install the OpenZFS utilities.
5. Ensure Omni runs with permission to set project quotas. If Omni itself runs
   in Docker, give that container access to the host utilities and quota
   interfaces.

No Omni configuration is required. On server creation and each configuration sync, Omni assigns the server data
directory a project ID and applies the filesystem-native hard limit to the
exact byte value
received from the Panel. A 20 GiB allocation therefore makes the kernel return
`ENOSPC` once `/home/container` reaches 20 GiB. A Panel value of `0` clears the
hard limit, preserving the existing unlimited-disk behaviour.

When enabled, an unsupported filesystem or a quota setup failure logs a warning
and Omni continues with its existing application-level disk accounting and
over-limit stop behaviour. This keeps a missing package or a node configuration
error from taking down the node, but is not equivalent to a kernel hard limit
for direct container writes.

Omni reads `/proc/self/mountinfo` to detect the filesystem, so detection does
not require the external `findmnt` command. The quota drivers do use host
utilities: ext4 needs `chattr` from `e2fsprogs` and `setquota` from `quota`;
XFS needs `xfs_quota` from `xfsprogs`. These are distribution packages, not
Linux-kernel commands. Btrfs uses `btrfs` from `btrfs-progs`. If a required
utility is absent, Omni logs the condition and uses the standard disk limiter.
ZFS uses the `zfs` utility supplied by OpenZFS.

For XFS, Omni passes the server directory to `xfs_quota` as the target
filesystem path. This is required by `xfs_quota` to select the mount that owns
the directory. Any diagnostic output emitted by the host utility is retained in
Omni's warning log, so missing `prjquota`, permissions, or package issues are
visible instead of appearing only as `exit status 1`.
