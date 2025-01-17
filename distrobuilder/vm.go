package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	lxd "github.com/lxc/lxd/shared"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"

	"github.com/lxc/distrobuilder/shared"
)

type vm struct {
	imageFile  string
	loopDevice string
	rootFS     string
	rootfsDir  string
	size       uint64
}

func newVM(imageFile, rootfsDir, fs string, size uint64) (*vm, error) {
	if fs == "" {
		fs = "ext4"
	}

	if !lxd.StringInSlice(fs, []string{"btrfs", "ext4"}) {
		return nil, errors.Errorf("Unsupported fs: %s", fs)
	}

	if size == 0 {
		size = 4294967296
	}

	return &vm{imageFile: imageFile, rootfsDir: rootfsDir, rootFS: fs, size: size}, nil
}

func (v *vm) getLoopDev() string {
	return v.loopDevice
}

func (v *vm) getRootfsDevFile() string {
	if v.loopDevice == "" {
		return ""
	}

	return fmt.Sprintf("%sp2", v.loopDevice)
}

func (v *vm) getUEFIDevFile() string {
	if v.loopDevice == "" {
		return ""
	}

	return fmt.Sprintf("%sp1", v.loopDevice)
}

func (v *vm) createEmptyDiskImage() error {
	f, err := os.Create(v.imageFile)
	if err != nil {
		return errors.Wrapf(err, "Failed to open %s", v.imageFile)
	}
	defer f.Close()

	err = f.Chmod(0600)
	if err != nil {
		return errors.Wrapf(err, "Failed to chmod %s", v.imageFile)
	}

	err = f.Truncate(int64(v.size))
	if err != nil {
		return errors.Wrapf(err, "Failed to create sparse file %s", v.imageFile)
	}

	return nil
}

func (v *vm) createPartitions() error {
	args := [][]string{
		{"--zap-all"},
		{"--new=1::+100M", "-t 1:EF00"},
		{"--new=2::", "-t 2:8300"},
	}

	for _, cmd := range args {
		err := shared.RunCommand("sgdisk", append([]string{v.imageFile}, cmd...)...)
		if err != nil {
			return errors.Wrap(err, "Failed to create partitions")
		}
	}

	return nil
}

func (v *vm) mountImage() error {
	// If loopDevice is set, it probably is already mounted.
	if v.loopDevice != "" {
		return nil
	}

	stdout, err := lxd.RunCommand("losetup", "-P", "-f", "--show", v.imageFile)
	if err != nil {
		return errors.Wrap(err, "Failed to setup loop device")
	}

	v.loopDevice = strings.TrimSpace(stdout)

	// Ensure the partitions are accessible. This part is usually only needed
	// if building inside of a container.

	out, err := lxd.RunCommand("lsblk", "--raw", "--output", "MAJ:MIN", "--noheadings", v.loopDevice)
	if err != nil {
		return errors.Wrap(err, "Failed to list block devices")
	}

	deviceNumbers := strings.Split(out, "\n")

	if !lxd.PathExists(v.getUEFIDevFile()) {
		fields := strings.Split(deviceNumbers[1], ":")

		major, err := strconv.Atoi(fields[0])
		if err != nil {
			return errors.Wrapf(err, "Failed to parse %q", fields[0])
		}

		minor, err := strconv.Atoi(fields[1])
		if err != nil {
			return errors.Wrapf(err, "Failed to parse %q", fields[1])
		}

		dev := unix.Mkdev(uint32(major), uint32(minor))

		err = unix.Mknod(v.getUEFIDevFile(), unix.S_IFBLK|0644, int(dev))
		if err != nil {
			return errors.Wrapf(err, "Failed to create block device %q", v.getUEFIDevFile())
		}
	}

	if !lxd.PathExists(v.getRootfsDevFile()) {
		fields := strings.Split(deviceNumbers[2], ":")

		major, err := strconv.Atoi(fields[0])
		if err != nil {
			return errors.Wrapf(err, "Failed to parse %q", fields[0])
		}

		minor, err := strconv.Atoi(fields[1])
		if err != nil {
			return errors.Wrapf(err, "Failed to parse %q", fields[1])
		}

		dev := unix.Mkdev(uint32(major), uint32(minor))

		err = unix.Mknod(v.getRootfsDevFile(), unix.S_IFBLK|0644, int(dev))
		if err != nil {
			return errors.Wrapf(err, "Failed to create block device %q", v.getRootfsDevFile())
		}
	}

	return nil
}

func (v *vm) umountImage() error {
	// If loopDevice is empty, the image probably isn't mounted.
	if v.loopDevice == "" || !lxd.PathExists(v.loopDevice) {
		return nil
	}

	err := shared.RunCommand("losetup", "-d", v.loopDevice)
	if err != nil {
		return errors.Wrap(err, "Failed to detach loop device")
	}

	// Make sure that p1 and p2 are also removed.
	if lxd.PathExists(v.getUEFIDevFile()) {
		err := os.Remove(v.getUEFIDevFile())
		if err != nil {
			return errors.Wrapf(err, "Failed to remove file %q", v.getUEFIDevFile())
		}
	}

	if lxd.PathExists(v.getRootfsDevFile()) {
		err := os.Remove(v.getRootfsDevFile())
		if err != nil {
			return errors.Wrapf(err, "Failed to remove file %q", v.getRootfsDevFile())
		}
	}

	v.loopDevice = ""

	return nil
}

func (v *vm) createRootFS() error {
	if v.loopDevice == "" {
		return errors.Errorf("Disk image not mounted")
	}

	switch v.rootFS {
	case "btrfs":
		err := shared.RunCommand("mkfs.btrfs", "-f", "-L", "rootfs", v.getRootfsDevFile())
		if err != nil {
			return errors.Wrap(err, "Failed to create btrfs filesystem")
		}

		// Create the root subvolume as well
		err = shared.RunCommand("mount", v.getRootfsDevFile(), v.rootfsDir)
		if err != nil {
			return errors.Wrapf(err, "Failed to mount %q at %q", v.getRootfsDevFile(), v.rootfsDir)
		}
		defer shared.RunCommand("umount", v.rootfsDir)

		return shared.RunCommand("btrfs", "subvolume", "create", fmt.Sprintf("%s/@", v.rootfsDir))
	case "ext4":
		return shared.RunCommand("mkfs.ext4", "-F", "-b", "4096", "-i 8192", "-m", "0", "-L", "rootfs", "-E", "resize=536870912", v.getRootfsDevFile())
	}

	return nil
}

func (v *vm) createUEFIFS() error {
	if v.loopDevice == "" {
		return errors.New("Disk image not mounted")
	}

	return shared.RunCommand("mkfs.vfat", "-F", "32", "-n", "UEFI", v.getUEFIDevFile())
}

func (v *vm) getRootfsPartitionUUID() (string, error) {
	if v.loopDevice == "" {
		return "", errors.New("Disk image not mounted")
	}

	stdout, err := lxd.RunCommand("blkid", "-s", "PARTUUID", "-o", "value", v.getRootfsDevFile())
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(stdout), nil
}

func (v *vm) getUEFIPartitionUUID() (string, error) {
	if v.loopDevice == "" {
		return "", errors.New("Disk image not mounted")
	}

	stdout, err := lxd.RunCommand("blkid", "-s", "PARTUUID", "-o", "value", v.getUEFIDevFile())
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(stdout), nil
}

func (v *vm) mountRootPartition() error {
	if v.loopDevice == "" {
		return errors.New("Disk image not mounted")
	}

	switch v.rootFS {
	case "btrfs":
		return shared.RunCommand("mount", v.getRootfsDevFile(), v.rootfsDir, "-o", "defaults,subvol=/@")
	case "ext4":
		return shared.RunCommand("mount", v.getRootfsDevFile(), v.rootfsDir)

	}

	return nil
}

func (v *vm) mountUEFIPartition() error {
	if v.loopDevice == "" {
		return errors.New("Disk image not mounted")
	}

	mountpoint := filepath.Join(v.rootfsDir, "boot", "efi")

	err := os.MkdirAll(mountpoint, 0755)
	if err != nil {
		return errors.Wrapf(err, "Failed to create directory %q", mountpoint)
	}

	return shared.RunCommand("mount", v.getUEFIDevFile(), mountpoint)
}
