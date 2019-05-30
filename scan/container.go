/* Vuls - Vulnerability Scanner
Copyright (C) 2016  Future Corporation , Japan.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package scan

import (
	"context"
	"fmt"
	"time"

	"github.com/knqyf263/fanal/analyzer"
	"golang.org/x/xerrors"

	"github.com/future-architect/vuls/config"
	"github.com/future-architect/vuls/models"
	"github.com/future-architect/vuls/util"
	fanalos "github.com/knqyf263/fanal/analyzer/os"
	godeptypes "github.com/knqyf263/go-dep-parser/pkg/types"

	// Register os and package analyzers
	_ "github.com/knqyf263/fanal/analyzer/os/alpine"
	_ "github.com/knqyf263/fanal/analyzer/os/amazonlinux"
	_ "github.com/knqyf263/fanal/analyzer/os/debianbase"
	_ "github.com/knqyf263/fanal/analyzer/os/opensuse"
	_ "github.com/knqyf263/fanal/analyzer/os/redhatbase"

	_ "github.com/knqyf263/fanal/analyzer/pkg/apk"
	_ "github.com/knqyf263/fanal/analyzer/pkg/dpkg"
	_ "github.com/knqyf263/fanal/analyzer/pkg/rpmcmd"

	_ "github.com/knqyf263/fanal/analyzer/library/bundler"
	_ "github.com/knqyf263/fanal/analyzer/library/cargo"
	_ "github.com/knqyf263/fanal/analyzer/library/composer"
	_ "github.com/knqyf263/fanal/analyzer/library/npm"
	_ "github.com/knqyf263/fanal/analyzer/library/pipenv"
	_ "github.com/knqyf263/fanal/analyzer/library/poetry"
	_ "github.com/knqyf263/fanal/analyzer/library/yarn"
)

// inherit OsTypeInterface
type image struct {
	base
}

func detectContainerImage(c config.ServerInfo) (itsMe bool, containerImage osTypeInterface, err error) {
	os, pkgs, libs, err := scanImage(c)
	if err != nil {
		// use Alpine for setErrs
		return false, newAlpine(c), err
	}
	switch os.Family {
	case fanalos.OpenSUSELeap, fanalos.OpenSUSETumbleweed, fanalos.OpenSUSE:
		containerImage = newAlpine(c)
		return false, containerImage, xerrors.Errorf("Unsupported OS : %s", os.Family)
	}

	libScanners, err := convertLibWithScanner(libs)
	if err != nil {
		return false, newAlpine(c), err
	}

	p := newContainerImage(c, pkgs, libScanners)
	p.setDistro(os.Family, os.Name)
	fmt.Println(p)
	return true, p, nil
}

func convertLibWithScanner(libs map[analyzer.FilePath][]godeptypes.Library) ([]models.LibraryScanner, error) {
	scanners := []models.LibraryScanner{}
	for path, pkgs := range libs {
		scanners = append(scanners, models.LibraryScanner{Path: string(path), Libs: pkgs})
	}
	return scanners, nil
}

// scanImage returns os, packages on image layers
func scanImage(c config.ServerInfo) (os *analyzer.OS, pkgs []analyzer.Package, libs map[analyzer.FilePath][]godeptypes.Library, err error) {
	if err = config.IsValidImage(c.Image); err != nil {
		return nil, nil, nil, err
	}

	ctx := context.Background()
	domain := c.Image.Name + ":" + c.Image.Tag
	util.Log.Info("Start fetch container... ", domain)

	// Configure dockerOption
	dockerOption := c.Image.DockerOption
	if dockerOption.Timeout == 0 {
		dockerOption.Timeout = 600 * time.Second
	}
	files, err := analyzer.Analyze(ctx, domain, dockerOption)

	if err != nil {
		return nil, nil, nil, xerrors.Errorf("Failed scan files %q, %w", domain, err)
	}

	containerOs, err := analyzer.GetOS(files)
	if err != nil {
		return nil, nil, nil, xerrors.Errorf("Failed scan os %q, %w", domain, err)
	}

	pkgs, err = analyzer.GetPackages(files)
	if err != nil {
		return nil, nil, nil, xerrors.Errorf("Failed scan pkgs %q, %w", domain, err)
	}
	libs, err = analyzer.GetLibraries(files)
	if err != nil {
		return nil, nil, nil, xerrors.Errorf("Failed scan libs %q, %w", domain, err)
	}
	return &containerOs, pkgs, libs, nil
}

func convertFanalToVulsPkg(pkgs []analyzer.Package) (map[string]models.Package, map[string]models.SrcPackage) {
	modelPkgs := map[string]models.Package{}
	modelSrcPkgs := map[string]models.SrcPackage{}
	for _, pkg := range pkgs {
		version := pkg.Version
		if pkg.Epoch != 0 {
			version = fmt.Sprintf("%d:%s", pkg.Epoch, pkg.Version)
		}
		modelPkgs[pkg.Name] = models.Package{
			Name:    pkg.Name,
			Release: pkg.Release,
			Version: version,
			Arch:    pkg.Arch,
		}

		// add SrcPacks
		if pkg.Name != pkg.SrcName {
			if pack, ok := modelSrcPkgs[pkg.SrcName]; ok {
				pack.AddBinaryName(pkg.Name)
				modelSrcPkgs[pkg.SrcName] = pack
			} else {
				modelSrcPkgs[pkg.SrcName] = models.SrcPackage{
					Name:        pkg.SrcName,
					Version:     pkg.SrcVersion,
					BinaryNames: []string{pkg.Name},
				}
			}
		}
	}
	return modelPkgs, modelSrcPkgs
}

func newContainerImage(c config.ServerInfo, pkgs []analyzer.Package, libs []models.LibraryScanner) *image {
	modelPkgs, modelSrcPkgs := convertFanalToVulsPkg(pkgs)
	d := &image{
		base: base{
			osPackages: osPackages{
				Packages:    modelPkgs,
				SrcPackages: modelSrcPkgs,
				VulnInfos:   models.VulnInfos{},
			},
			LibraryScanners: libs,
		},
	}
	d.log = util.NewCustomLogger(c)
	d.setServerInfo(c)
	return d
}

func (o *image) checkScanMode() error {
	return nil
}

func (o *image) checkIfSudoNoPasswd() error {
	return nil
}

func (o *image) checkDeps() error {
	return nil
}

func (o *image) preCure() error {
	return nil
}

func (o *image) postScan() error {
	return nil
}

func (o *image) scanPackages() error {
	return nil
}

func (o *image) parseInstalledPackages(string) (models.Packages, models.SrcPackages, error) {
	return nil, nil, nil
}

func (o *image) detectPlatform() {
	o.setPlatform(models.Platform{Name: "image"})
}
