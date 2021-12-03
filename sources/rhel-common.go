package sources

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lxc/distrobuilder/shared"
	lxd "github.com/lxc/lxd/shared"
	"golang.org/x/sys/unix"
)

type commonRHEL struct {
	common
}

func (c *commonRHEL) unpackISO(filePath, rootfsDir string, scriptRunner func(string) error) error {
	isoDir, err := ioutil.TempDir(c.cacheDir, "temp_")
	if err != nil {
		return fmt.Errorf("Failed to create temporary directory: %w", err)
	}
	defer os.RemoveAll(isoDir)

	squashfsDir, err := ioutil.TempDir(c.cacheDir, "temp_")
	if err != nil {
		return fmt.Errorf("Failed to create temporary directory: %w", err)
	}
	defer os.RemoveAll(squashfsDir)

	tempRootDir, err := ioutil.TempDir(c.cacheDir, "temp_")
	if err != nil {
		return fmt.Errorf("Failed to create temporary directory: %w", err)
	}
	defer os.RemoveAll(tempRootDir)

	// this is easier than doing the whole loop thing ourselves
	err = shared.RunCommand("mount", "-o", "ro", filePath, isoDir)
	if err != nil {
		return fmt.Errorf("Failed to mount %q: %w", filePath, err)
	}
	defer unix.Unmount(isoDir, 0)

	var rootfsImage string
	squashfsImage := filepath.Join(isoDir, "LiveOS", "squashfs.img")
	if lxd.PathExists(squashfsImage) {
		// The squashfs.img contains an image containing the rootfs, so first
		// mount squashfs.img
		err = shared.RunCommand("mount", "-o", "ro", squashfsImage, squashfsDir)
		if err != nil {
			return fmt.Errorf("Failed to mount %q: %w", squashfsImage, err)
		}
		defer unix.Unmount(squashfsDir, 0)

		rootfsImage = filepath.Join(squashfsDir, "LiveOS", "rootfs.img")
	} else {
		rootfsImage = filepath.Join(isoDir, "images", "install.img")
	}

	// Remove rootfsDir otherwise rsync will copy the content into the directory
	// itself
	err = os.RemoveAll(rootfsDir)
	if err != nil {
		return fmt.Errorf("Failed to remove directory %q: %w", rootfsDir, err)
	}

	c.logger.Infow("Unpacking root image", "file", rootfsImage)

	err = c.unpackRootfsImage(rootfsImage, tempRootDir)
	if err != nil {
		return fmt.Errorf("Failed to unpack %q: %w", rootfsImage, err)
	}

	gpgKeysPath := ""

	packagesDir := filepath.Join(isoDir, "Packages")
	repodataDir := filepath.Join(isoDir, "repodata")

	if !lxd.PathExists(packagesDir) {
		packagesDir = filepath.Join(isoDir, "BaseOS", "Packages")
	}
	if !lxd.PathExists(repodataDir) {
		repodataDir = filepath.Join(isoDir, "BaseOS", "repodata")
	}

	if lxd.PathExists(packagesDir) && lxd.PathExists(repodataDir) {
		// Create cdrom repo for yum
		err = os.MkdirAll(filepath.Join(tempRootDir, "mnt", "cdrom"), 0755)
		if err != nil {
			return fmt.Errorf("Failed to create directory %q: %w", filepath.Join(tempRootDir, "mnt", "cdrom"), err)
		}

		// Copy repo relevant files to the cdrom
		err = shared.RsyncLocal(packagesDir, filepath.Join(tempRootDir, "mnt", "cdrom"))
		if err != nil {
			return fmt.Errorf("Failed to copy Packages: %w", err)
		}

		err = shared.RsyncLocal(repodataDir, filepath.Join(tempRootDir, "mnt", "cdrom"))
		if err != nil {
			return fmt.Errorf("Failed to copy repodata: %w", err)
		}

		// Find all relevant GPG keys
		gpgKeys, err := filepath.Glob(filepath.Join(isoDir, "RPM-GPG-KEY-*"))
		if err != nil {
			return fmt.Errorf("Failed to match gpg keys: %w", err)
		}

		// Copy the keys to the cdrom
		for _, key := range gpgKeys {
			fmt.Printf("key=%v\n", key)
			if len(gpgKeysPath) > 0 {
				gpgKeysPath += " "
			}
			gpgKeysPath += fmt.Sprintf("file:///mnt/cdrom/%s", filepath.Base(key))

			err = shared.RsyncLocal(key, filepath.Join(tempRootDir, "mnt", "cdrom"))
			if err != nil {
				return fmt.Errorf(`Failed to run "rsync": %w`, err)
			}
		}
	}

	// Setup the mounts and chroot into the rootfs
	exitChroot, err := shared.SetupChroot(tempRootDir, shared.DefinitionEnv{}, nil)
	if err != nil {
		return fmt.Errorf("Failed to setup chroot: %w", err)
	}

	err = scriptRunner(gpgKeysPath)
	if err != nil {
		exitChroot()
		return fmt.Errorf("Failed to run script: %w", err)
	}

	exitChroot()

	err = shared.RsyncLocal(tempRootDir+"/rootfs/", rootfsDir)
	if err != nil {
		return fmt.Errorf(`Failed to run "rsync": %w`, err)
	}

	return nil
}

func (c *commonRHEL) unpackRootfsImage(imageFile string, target string) error {
	installDir, err := ioutil.TempDir(c.cacheDir, "temp_")
	if err != nil {
		return fmt.Errorf("Failed to create temporary directory: %w", err)
	}
	defer os.RemoveAll(installDir)

	err = shared.RunCommand("mount", "-o", "ro", imageFile, installDir)
	if err != nil {
		return fmt.Errorf("Failed to mount %q: %w", imageFile, err)
	}
	defer unix.Unmount(installDir, 0)

	rootfsDir := installDir
	rootfsFile := filepath.Join(installDir, "LiveOS", "rootfs.img")

	if lxd.PathExists(rootfsFile) {
		rootfsDir, err = ioutil.TempDir(c.cacheDir, "temp_")
		if err != nil {
			return fmt.Errorf("Failed to create temporary directory: %w", err)
		}
		defer os.RemoveAll(rootfsDir)

		err = shared.RunCommand("mount", "-o", "ro", rootfsFile, rootfsDir)
		if err != nil {
			return fmt.Errorf("Failed to mount %q: %w", rootfsFile, err)
		}
		defer unix.Unmount(rootfsDir, 0)
	}

	// Since rootfs is read-only, we need to copy it to a temporary rootfs
	// directory in order to create the minimal rootfs.
	err = shared.RsyncLocal(rootfsDir+"/", target)
	if err != nil {
		return fmt.Errorf(`Failed to run "rsync": %w`, err)
	}

	return nil
}

func (c *commonRHEL) unpackRaw(filePath, rootfsDir string, scriptRunner func() error) error {
	roRootDir := filepath.Join(c.cacheDir, "rootfs.ro")
	tempRootDir := filepath.Join(c.cacheDir, "rootfs")

	err := os.MkdirAll(roRootDir, 0755)
	if err != nil {
		return fmt.Errorf("Failed to create directory %q: %w", roRootDir, err)
	}

	if strings.HasSuffix(filePath, ".raw.xz") {
		// Uncompress raw image
		err := shared.RunCommand("unxz", filePath)
		if err != nil {
			return fmt.Errorf(`Failed to run "unxz": %w`, err)
		}
	}

	rawFilePath := strings.TrimSuffix(filePath, ".xz")

	// Figure out the offset
	var buf bytes.Buffer

	err = lxd.RunCommandWithFds(nil, &buf, "fdisk", "-l", "-o", "Start", rawFilePath)
	if err != nil {
		return fmt.Errorf(`Failed to run "fdisk": %w`, err)
	}

	output := strings.Split(buf.String(), "\n")
	offsetStr := strings.TrimSpace(output[len(output)-2])

	offset, err := strconv.Atoi(offsetStr)
	if err != nil {
		return fmt.Errorf("Failed to convert %q: %w", offsetStr, err)
	}

	// Mount the partition read-only since we don't want to accidently modify it.
	err = shared.RunCommand("mount", "-o", fmt.Sprintf("ro,loop,offset=%d", offset*512),
		rawFilePath, roRootDir)
	if err != nil {
		return fmt.Errorf("Failed to mount %q: %w", rawFilePath, err)
	}
	defer unix.Unmount(roRootDir, 0)

	// Since roRootDir is read-only, we need to copy it to a temporary rootfs
	// directory in order to create the minimal rootfs.
	err = shared.RsyncLocal(roRootDir+"/", tempRootDir)
	if err != nil {
		return fmt.Errorf(`Failed to run "rsync": %w`, err)
	}

	// Setup the mounts and chroot into the rootfs
	exitChroot, err := shared.SetupChroot(tempRootDir, shared.DefinitionEnv{}, nil)
	if err != nil {
		return fmt.Errorf("Failed to setup chroot: %w", err)
	}

	err = scriptRunner()
	if err != nil {
		exitChroot()
		return fmt.Errorf("Failed to run script: %w", err)
	}

	exitChroot()

	err = shared.RsyncLocal(tempRootDir+"/rootfs/", rootfsDir)
	if err != nil {
		return fmt.Errorf(`Failed to run "rsync": %w`, err)
	}

	return nil
}
