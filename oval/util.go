// +build !scanner

package oval

import (
	"encoding/json"
	"net/http"
	"regexp"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/future-architect/vuls/config"
	"github.com/future-architect/vuls/constant"
	"github.com/future-architect/vuls/logging"
	"github.com/future-architect/vuls/models"
	"github.com/future-architect/vuls/util"
	apkver "github.com/knqyf263/go-apk-version"
	debver "github.com/knqyf263/go-deb-version"
	rpmver "github.com/knqyf263/go-rpm-version"
	"github.com/kotakanbe/goval-dictionary/db"
	ovalmodels "github.com/kotakanbe/goval-dictionary/models"
	"github.com/parnurzeal/gorequest"
	"golang.org/x/xerrors"
)

type ovalResult struct {
	entries []defPacks
}

type defPacks struct {
	def ovalmodels.Definition

	// BinaryPackageName : NotFixedYet
	binpkgFixstat map[string]fixStat
}

type fixStat struct {
	notFixedYet bool
	fixedIn     string
	isSrcPack   bool
	srcPackName string
}

func (e defPacks) toPackStatuses() (ps models.PackageFixStatuses) {
	for name, stat := range e.binpkgFixstat {
		ps = append(ps, models.PackageFixStatus{
			Name:        name,
			NotFixedYet: stat.notFixedYet,
			FixedIn:     stat.fixedIn,
		})
	}
	return
}

func (e *ovalResult) upsert(def ovalmodels.Definition, packName string, fstat fixStat) (upserted bool) {
	// alpine's entry is empty since Alpine secdb is not OVAL format
	if def.DefinitionID != "" {
		for i, entry := range e.entries {
			if entry.def.DefinitionID == def.DefinitionID {
				e.entries[i].binpkgFixstat[packName] = fstat
				return true
			}
		}
	}
	e.entries = append(e.entries, defPacks{
		def: def,
		binpkgFixstat: map[string]fixStat{
			packName: fstat,
		},
	})

	return false
}

type request struct {
	packName          string
	versionRelease    string
	newVersionRelease string
	arch              string
	binaryPackNames   []string
	isSrcPack         bool
	modularityLabel   string // RHEL 8 or later only
}

type response struct {
	request request
	defs    []ovalmodels.Definition
}

// getDefsByPackNameViaHTTP fetches OVAL information via HTTP
func getDefsByPackNameViaHTTP(r *models.ScanResult) (
	relatedDefs ovalResult, err error) {

	nReq := len(r.Packages) + len(r.SrcPackages)
	reqChan := make(chan request, nReq)
	resChan := make(chan response, nReq)
	errChan := make(chan error, nReq)
	defer close(reqChan)
	defer close(resChan)
	defer close(errChan)

	go func() {
		for _, pack := range r.Packages {
			reqChan <- request{
				packName:          pack.Name,
				versionRelease:    pack.FormatVer(),
				newVersionRelease: pack.FormatVer(),
				isSrcPack:         false,
				arch:              pack.Arch,
			}
		}
		for _, pack := range r.SrcPackages {
			reqChan <- request{
				packName:        pack.Name,
				binaryPackNames: pack.BinaryNames,
				versionRelease:  pack.Version,
				isSrcPack:       true,
				// arch:            pack.Arch,
			}
		}
	}()

	concurrency := 10
	tasks := util.GenWorkers(concurrency)
	for i := 0; i < nReq; i++ {
		tasks <- func() {
			select {
			case req := <-reqChan:
				url, err := util.URLPathJoin(
					config.Conf.OvalDict.URL,
					"packs",
					r.Family,
					r.Release,
					req.packName,
				)
				if err != nil {
					errChan <- err
				} else {
					logging.Log.Debugf("HTTP Request to %s", url)
					httpGet(url, req, resChan, errChan)
				}
			}
		}
	}

	timeout := time.After(2 * 60 * time.Second)
	var errs []error
	for i := 0; i < nReq; i++ {
		select {
		case res := <-resChan:
			for _, def := range res.defs {
				affected, notFixedYet, fixedIn := isOvalDefAffected(def, res.request, r.Family, r.RunningKernel, r.EnabledDnfModules)
				if !affected {
					continue
				}

				if res.request.isSrcPack {
					for _, n := range res.request.binaryPackNames {
						fs := fixStat{
							srcPackName: res.request.packName,
							isSrcPack:   true,
							notFixedYet: notFixedYet,
							fixedIn:     fixedIn,
						}
						relatedDefs.upsert(def, n, fs)
					}
				} else {
					fs := fixStat{
						notFixedYet: notFixedYet,
						fixedIn:     fixedIn,
					}
					relatedDefs.upsert(def, res.request.packName, fs)
				}
			}
		case err := <-errChan:
			errs = append(errs, err)
		case <-timeout:
			return relatedDefs, xerrors.New("Timeout Fetching OVAL")
		}
	}
	if len(errs) != 0 {
		return relatedDefs, xerrors.Errorf("Failed to fetch OVAL. err: %w", errs)
	}
	return
}

func httpGet(url string, req request, resChan chan<- response, errChan chan<- error) {
	var body string
	var errs []error
	var resp *http.Response
	count, retryMax := 0, 3
	f := func() (err error) {
		//  resp, body, errs = gorequest.New().SetDebug(config.Conf.Debug).Get(url).End()
		resp, body, errs = gorequest.New().Timeout(10 * time.Second).Get(url).End()
		if 0 < len(errs) || resp == nil || resp.StatusCode != 200 {
			count++
			if count == retryMax {
				return nil
			}
			return xerrors.Errorf("HTTP GET error, url: %s, resp: %v, err: %+v", url, resp, errs)
		}
		return nil
	}
	notify := func(err error, t time.Duration) {
		logging.Log.Warnf("Failed to HTTP GET. retrying in %s seconds. err: %+v", t, err)
	}
	err := backoff.RetryNotify(f, backoff.NewExponentialBackOff(), notify)
	if err != nil {
		errChan <- xerrors.Errorf("HTTP Error %w", err)
		return
	}
	if count == retryMax {
		errChan <- xerrors.New("HRetry count exceeded")
		return
	}

	defs := []ovalmodels.Definition{}
	if err := json.Unmarshal([]byte(body), &defs); err != nil {
		errChan <- xerrors.Errorf("Failed to Unmarshal. body: %s, err: %w", body, err)
		return
	}
	resChan <- response{
		request: req,
		defs:    defs,
	}
}

func getDefsByPackNameFromOvalDB(driver db.DB, r *models.ScanResult) (relatedDefs ovalResult, err error) {
	requests := []request{}
	for _, pack := range r.Packages {
		requests = append(requests, request{
			packName:          pack.Name,
			versionRelease:    pack.FormatVer(),
			newVersionRelease: pack.FormatNewVer(),
			arch:              pack.Arch,
			isSrcPack:         false,
		})
	}
	for _, pack := range r.SrcPackages {
		requests = append(requests, request{
			packName:        pack.Name,
			binaryPackNames: pack.BinaryNames,
			versionRelease:  pack.Version,
			arch:            pack.Arch,
			isSrcPack:       true,
		})
	}

	for _, req := range requests {
		definitions, err := driver.GetByPackName(r.Family, r.Release, req.packName, req.arch)
		if err != nil {
			return relatedDefs, xerrors.Errorf("Failed to get %s OVAL info by package: %#v, err: %w", r.Family, req, err)
		}
		for _, def := range definitions {
			affected, notFixedYet, fixedIn := isOvalDefAffected(def, req, r.Family, r.RunningKernel, r.EnabledDnfModules)
			if !affected {
				continue
			}

			if req.isSrcPack {
				for _, binName := range req.binaryPackNames {
					fs := fixStat{
						notFixedYet: false,
						isSrcPack:   true,
						fixedIn:     fixedIn,
						srcPackName: req.packName,
					}
					relatedDefs.upsert(def, binName, fs)
				}
			} else {
				fs := fixStat{
					notFixedYet: notFixedYet,
					fixedIn:     fixedIn,
				}
				relatedDefs.upsert(def, req.packName, fs)
			}
		}
	}
	return
}

func isOvalDefAffected(def ovalmodels.Definition, req request, family string, running models.Kernel, enabledMods []string) (affected, notFixedYet bool, fixedIn string) {
	for _, ovalPack := range def.AffectedPacks {
		if req.packName != ovalPack.Name {
			continue
		}

		isModularityLabelEmptyOrSame := false
		if ovalPack.ModularityLabel != "" {
			for _, mod := range enabledMods {
				if mod == ovalPack.ModularityLabel {
					isModularityLabelEmptyOrSame = true
					break
				}
			}
		} else {
			isModularityLabelEmptyOrSame = true
		}
		if !isModularityLabelEmptyOrSame {
			continue
		}

		if running.Release != "" {
			switch family {
			case constant.RedHat, constant.CentOS:
				// For kernel related packages, ignore OVAL information with different major versions
				if _, ok := kernelRelatedPackNames[ovalPack.Name]; ok {
					if util.Major(ovalPack.Version) != util.Major(running.Release) {
						continue
					}
				}
			}
		}

		if ovalPack.NotFixedYet {
			return true, true, ovalPack.Version
		}

		// Compare between the installed version vs the version in OVAL
		less, err := lessThan(family, req.versionRelease, ovalPack)
		if err != nil {
			logging.Log.Debugf("Failed to parse versions: %s, Ver: %#v, OVAL: %#v, DefID: %s",
				err, req.versionRelease, ovalPack, def.DefinitionID)
			return false, false, ovalPack.Version
		}
		if less {
			if req.isSrcPack {
				// Unable to judge whether fixed or not-fixed of src package(Ubuntu, Debian)
				return true, false, ovalPack.Version
			}

			// If the version of installed is less than in OVAL
			switch family {
			case constant.RedHat,
				constant.Amazon,
				constant.SUSEEnterpriseServer,
				constant.Debian,
				constant.Ubuntu,
				constant.Raspbian:
				// Use fixed state in OVAL for these distros.
				return true, false, ovalPack.Version
			}

			// But CentOS can't judge whether fixed or unfixed.
			// Because fixed state in RHEL OVAL is different.
			// So, it have to be judged version comparison.

			// `offline` or `fast` scan mode can't get a updatable version.
			// In these mode, the blow field was set empty.
			// Vuls can not judge fixed or unfixed.
			if req.newVersionRelease == "" {
				return true, false, ovalPack.Version
			}

			// compare version: newVer vs oval
			less, err := lessThan(family, req.newVersionRelease, ovalPack)
			if err != nil {
				logging.Log.Debugf("Failed to parse versions: %s, NewVer: %#v, OVAL: %#v, DefID: %s",
					err, req.newVersionRelease, ovalPack, def.DefinitionID)
				return false, false, ovalPack.Version
			}
			return true, less, ovalPack.Version
		}
	}
	return false, false, ""
}

func lessThan(family, newVer string, packInOVAL ovalmodels.Package) (bool, error) {
	switch family {
	case constant.Debian,
		constant.Ubuntu,
		constant.Raspbian:
		vera, err := debver.NewVersion(newVer)
		if err != nil {
			return false, err
		}
		verb, err := debver.NewVersion(packInOVAL.Version)
		if err != nil {
			return false, err
		}
		return vera.LessThan(verb), nil

	case constant.Alpine:
		vera, err := apkver.NewVersion(newVer)
		if err != nil {
			return false, err
		}
		verb, err := apkver.NewVersion(packInOVAL.Version)
		if err != nil {
			return false, err
		}
		return vera.LessThan(verb), nil

	case constant.Oracle,
		constant.SUSEEnterpriseServer,
		constant.Amazon:
		vera := rpmver.NewVersion(newVer)
		verb := rpmver.NewVersion(packInOVAL.Version)
		return vera.LessThan(verb), nil

	case constant.RedHat,
		constant.CentOS:
		vera := rpmver.NewVersion(centOSVersionToRHEL(newVer))
		verb := rpmver.NewVersion(centOSVersionToRHEL(packInOVAL.Version))
		return vera.LessThan(verb), nil

	default:
		return false, xerrors.Errorf("Not implemented yet: %s", family)
	}
}

var centosVerPattern = regexp.MustCompile(`\.[es]l(\d+)(?:_\d+)?(?:\.centos)?`)

func centOSVersionToRHEL(ver string) string {
	return centosVerPattern.ReplaceAllString(ver, ".el$1")
}
