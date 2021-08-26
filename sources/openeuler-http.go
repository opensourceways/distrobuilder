package sources

import (
	"crypto/sha256"
	"fmt"
	"github.com/pkg/errors"
	"net/url"
	"path/filepath"

	"github.com/lxc/distrobuilder/shared"
)

type openEuler struct {
	commonRHEL

	fileName     string
	checksumFile string
}

const (
	isoFileName = "%s-%s-dvd.iso"
	shaFileName = "%s-%s-dvd.iso.sha256num"
)

func (s *openEuler) Run() error {
	var err error

	baseURL := fmt.Sprintf("%s/%s/ISO/%s/", s.definition.Source.URL,
		s.definition.Image.Release,
		s.definition.Image.Architecture)

	fpath := shared.GetTargetDir(s.definition.Image)
	s.fileName = fmt.Sprintf(isoFileName, s.definition.Image.Name, s.definition.Image.Architecture)
	s.checksumFile = fmt.Sprintf(shaFileName, s.definition.Image.Name, s.definition.Image.Architecture)

	_, err = url.Parse(baseURL)
	if err != nil {
		return errors.Wrapf(err, "Failed to parse URL %q", baseURL)
	}
	//s.definition.Source.SkipVerification ignored here

	_, err = shared.DownloadHash(s.definition.Image, baseURL+s.fileName, s.checksumFile, sha256.New())
	if err != nil {
		return errors.Wrapf(err, "Failed to download %q", baseURL+s.fileName)
	}

	source := filepath.Join(fpath, s.fileName)

	s.logger.Infow("Unpacking image", "file", source)

	err = s.unpackISO(source, s.rootfsDir, s.isoRunner)
	if err != nil {
		return errors.Wrapf(err, "Failed to unpack %q", source)
	}
	return nil
}

func (s *openEuler) isoRunner(gpgKeysPath string) error {
	err := shared.RunScript(fmt.Sprintf(`#!/bin/sh
set -eux

GPG_KEYS="%s"

# Create required files
#TODO(tommylikehu): why we need /etc/mtab
touch /etc/mtab /etc/fstab

yum_args=""
mkdir -p /etc/yum.repos.d

# NOTE(tommylikehu): for openEuler packageDir and repoDir always exist.
# Install initial package set
cd /mnt/cdrom/Packages
rpm -ivh --nodeps $(ls rpm-*.rpm | head -n1)
rpm -ivh --nodeps $(ls yum-*.rpm | head -n1)

# Add cdrom repo
cat <<- EOF > /etc/yum.repos.d/cdrom.repo
[cdrom]
name=Install CD-ROM
baseurl=file:///mnt/cdrom
enabled=0
EOF

gpg_keys_official="file:///etc/pki/rpm-gpg/RPM-GPG-KEY-openEuler"

if [ -n "${GPG_KEYS}" ]; then
	echo gpgcheck=1 >> /etc/yum.repos.d/cdrom.repo
	echo gpgkey=${gpg_keys_official} ${GPG_KEYS} >> /etc/yum.repos.d/cdrom.repo
else
	echo gpgcheck=0 >> /etc/yum.repos.d/cdrom.repo
fi

yum_args="--disablerepo=* --enablerepo=cdrom"
yum ${yum_args} -y reinstall yum

pkgs="basesystem openEuler-release yum"

# Create a minimal rootfs, #TODO: releaseversion is not set in yum command
mkdir /rootfs
yum ${yum_args} --installroot=/rootfs -y  --skip-broken install ${pkgs}
rm -rf /rootfs/var/cache/yum
`, gpgKeysPath))
	if err != nil {
		return errors.Wrap(err, "Failed to run script")
	}

	return nil
}