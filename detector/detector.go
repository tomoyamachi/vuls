// +build !scanner

package detector

import (
	"os"
	"strings"
	"time"

	c "github.com/future-architect/vuls/config"
	"github.com/future-architect/vuls/constant"
	"github.com/future-architect/vuls/contrib/owasp-dependency-check/parser"
	"github.com/future-architect/vuls/cwe"
	"github.com/future-architect/vuls/exploit"
	"github.com/future-architect/vuls/gost"
	"github.com/future-architect/vuls/logging"
	"github.com/future-architect/vuls/models"
	"github.com/future-architect/vuls/msf"
	"github.com/future-architect/vuls/oval"
	"github.com/future-architect/vuls/reporter"
	"github.com/future-architect/vuls/util"
	gostdb "github.com/knqyf263/gost/db"
	cvedb "github.com/kotakanbe/go-cve-dictionary/db"
	cvemodels "github.com/kotakanbe/go-cve-dictionary/models"
	ovaldb "github.com/kotakanbe/goval-dictionary/db"
	metasploitdb "github.com/takuzoo3868/go-msfdb/db"
	exploitdb "github.com/vulsio/go-exploitdb/db"
	"golang.org/x/xerrors"
)

// type Detector struct {
// 	Targets map[string]c.ServerInfo
// }

// Detect vulns and fill CVE detailed information
func Detect(dbclient DBClient, rs []models.ScanResult, dir string) ([]models.ScanResult, error) {

	// Use the same reportedAt for all rs
	reportedAt := time.Now()
	for i, r := range rs {
		if !c.Conf.RefreshCve && !needToRefreshCve(r) {
			logging.Log.Info("No need to refresh")
			continue
		}

		if !reuseScannedCves(&r) {
			r.ScannedCves = models.VulnInfos{}
		}

		cpeURIs, owaspDCXMLPath := []string{}, ""
		if len(r.Container.ContainerID) == 0 {
			cpeURIs = c.Conf.Servers[r.ServerName].CpeNames
			owaspDCXMLPath = c.Conf.Servers[r.ServerName].OwaspDCXMLPath
		} else {
			if s, ok := c.Conf.Servers[r.ServerName]; ok {
				if con, ok := s.Containers[r.Container.Name]; ok {
					cpeURIs = con.Cpes
					owaspDCXMLPath = con.OwaspDCXMLPath
				}
			}
		}
		if owaspDCXMLPath != "" {
			cpes, err := parser.Parse(owaspDCXMLPath)
			if err != nil {
				return nil, xerrors.Errorf("Failed to read OWASP Dependency Check XML on %s, `%s`, err: %w",
					r.ServerInfo(), owaspDCXMLPath, err)
			}
			cpeURIs = append(cpeURIs, cpes...)
		}

		if err := DetectLibsCves(&r, c.Conf.TrivyCacheDBDir, c.Conf.NoProgress); err != nil {
			return nil, xerrors.Errorf("Failed to fill with Library dependency: %w", err)
		}

		if err := DetectPkgCves(dbclient, &r); err != nil {
			return nil, xerrors.Errorf("Failed to detect Pkg CVE: %w", err)
		}

		if err := DetectCpeURIsCves(dbclient.CveDB, &r, cpeURIs); err != nil {
			return nil, xerrors.Errorf("Failed to detect CVE of `%s`: %w", cpeURIs, err)
		}

		repos := c.Conf.Servers[r.ServerName].GitHubRepos
		if err := DetectGitHubCves(&r, repos, c.Conf.IgnoreGitHubDismissed); err != nil {
			return nil, xerrors.Errorf("Failed to detect GitHub Cves: %w", err)
		}

		if err := DetectWordPressCves(&r, &c.Conf.WpScan); err != nil {
			return nil, xerrors.Errorf("Failed to detect WordPress Cves: %w", err)
		}

		if err := FillCveInfo(dbclient, &r); err != nil {
			return nil, err
		}

		r.ReportedBy, _ = os.Hostname()
		r.Lang = c.Conf.Lang
		r.ReportedAt = reportedAt
		r.ReportedVersion = c.Version
		r.ReportedRevision = c.Revision
		r.Config.Report = c.Conf
		r.Config.Report.Servers = map[string]c.ServerInfo{
			r.ServerName: c.Conf.Servers[r.ServerName],
		}
		rs[i] = r
	}

	// Overwrite the json file every time to clear the fields specified in config.IgnoredJSONKeys
	for _, r := range rs {
		if s, ok := c.Conf.Servers[r.ServerName]; ok {
			r = r.ClearFields(s.IgnoredJSONKeys)
		}
		//TODO don't call here
		if err := reporter.OverwriteJSONFile(dir, r); err != nil {
			return nil, xerrors.Errorf("Failed to write JSON: %w", err)
		}
	}

	if c.Conf.DiffPlus || c.Conf.DiffMinus {
		prevs, err := loadPrevious(rs)
		if err != nil {
			return nil, err
		}
		rs = diff(rs, prevs, c.Conf.DiffPlus, c.Conf.DiffMinus)
	}

	for i, r := range rs {
		r.ScannedCves = r.ScannedCves.FilterByCvssOver(c.Conf.CvssScoreOver)
		r.ScannedCves = r.ScannedCves.FilterUnfixed(c.Conf.IgnoreUnfixed)

		// IgnoreCves
		ignoreCves := []string{}
		if r.Container.Name == "" {
			ignoreCves = c.Conf.Servers[r.ServerName].IgnoreCves
		} else if con, ok := c.Conf.Servers[r.ServerName].Containers[r.Container.Name]; ok {
			ignoreCves = con.IgnoreCves
		}
		r.ScannedCves = r.ScannedCves.FilterIgnoreCves(ignoreCves)

		// ignorePkgs
		ignorePkgsRegexps := []string{}
		if r.Container.Name == "" {
			ignorePkgsRegexps = c.Conf.Servers[r.ServerName].IgnorePkgsRegexp
		} else if s, ok := c.Conf.Servers[r.ServerName].Containers[r.Container.Name]; ok {
			ignorePkgsRegexps = s.IgnorePkgsRegexp
		}
		r.ScannedCves = r.ScannedCves.FilterIgnorePkgs(ignorePkgsRegexps)

		// IgnoreUnscored
		if c.Conf.IgnoreUnscoredCves {
			r.ScannedCves = r.ScannedCves.FindScoredVulns()
		}

		r.FilterInactiveWordPressLibs(c.Conf.WpScan.DetectInactive)
		rs[i] = r
	}
	return rs, nil
}

// DetectPkgCves detects OS pkg cves
func DetectPkgCves(dbclient DBClient, r *models.ScanResult) error {
	// Pkg Scan
	if r.Release != "" {
		// OVAL
		if err := detectPkgsCvesWithOval(dbclient.OvalDB, r); err != nil {
			return xerrors.Errorf("Failed to detect CVE with OVAL: %w", err)
		}

		// gost
		if err := detectPkgsCvesWithGost(dbclient.GostDB, r); err != nil {
			return xerrors.Errorf("Failed to detect CVE with gost: %w", err)
		}
	} else if reuseScannedCves(r) {
		logging.Log.Infof("r.Release is empty. Use CVEs as it as.")
	} else if r.Family == constant.ServerTypePseudo {
		logging.Log.Infof("pseudo type. Skip OVAL and gost detection")
	} else {
		return xerrors.Errorf("Failed to fill CVEs. r.Release is empty")
	}

	for i, v := range r.ScannedCves {
		for j, p := range v.AffectedPackages {
			if p.NotFixedYet && p.FixState == "" {
				p.FixState = "Not fixed yet"
				r.ScannedCves[i].AffectedPackages[j] = p
			}
		}
	}

	// To keep backward compatibility
	// Newer versions use ListenPortStats,
	// but older versions of Vuls are set to ListenPorts.
	// Set ListenPorts to ListenPortStats to allow newer Vuls to report old results.
	for i, pkg := range r.Packages {
		for j, proc := range pkg.AffectedProcs {
			for _, ipPort := range proc.ListenPorts {
				ps, err := models.NewPortStat(ipPort)
				if err != nil {
					logging.Log.Warnf("Failed to parse ip:port: %s, err:%+v", ipPort, err)
					continue
				}
				r.Packages[i].AffectedProcs[j].ListenPortStats = append(
					r.Packages[i].AffectedProcs[j].ListenPortStats, *ps)
			}
		}
	}

	return nil
}

// DetectGitHubCves fetches CVEs from GitHub Security Alerts
func DetectGitHubCves(r *models.ScanResult, githubConfs map[string]c.GitHubConf, ignoreDismissed bool) error {
	if len(githubConfs) == 0 {
		return nil
	}
	for ownerRepo, setting := range githubConfs {
		ss := strings.Split(ownerRepo, "/")
		if len(ss) != 2 {
			return xerrors.Errorf("Failed to parse GitHub owner/repo: %s", ownerRepo)
		}
		owner, repo := ss[0], ss[1]
		n, err := DetectGitHubSecurityAlerts(r, owner, repo, setting.Token, ignoreDismissed)
		if err != nil {
			return xerrors.Errorf("Failed to access GitHub Security Alerts: %w", err)
		}
		logging.Log.Infof("%s: %d CVEs detected with GHSA %s/%s",
			r.FormatServerName(), n, owner, repo)
	}
	return nil
}

// DetectWordPressCves detects CVEs of WordPress
func DetectWordPressCves(r *models.ScanResult, wpCnf *c.WpScanConf) error {
	if len(r.WordPressPackages) == 0 {
		return nil
	}
	logging.Log.Infof("Detect WordPress CVE. pkgs: %d ", len(r.WordPressPackages))
	n, err := detectWordPressCves(r, wpCnf)
	if err != nil {
		return xerrors.Errorf("Failed to detect WordPress CVE: %w", err)
	}
	logging.Log.Infof("%s: found %d WordPress CVEs", r.FormatServerName(), n)
	return nil
}

// FillCveInfo fill scanResult with cve info.
func FillCveInfo(dbclient DBClient, r *models.ScanResult) error {
	logging.Log.Infof("Fill CVE detailed with gost")
	if err := gost.NewClient(r.Family).FillCVEsWithRedHat(dbclient.GostDB, r); err != nil {
		return xerrors.Errorf("Failed to fill with gost: %w", err)
	}

	logging.Log.Infof("Fill CVE detailed with CVE-DB")
	if err := fillCvesWithNvdJvn(dbclient.CveDB, r); err != nil {
		return xerrors.Errorf("Failed to fill with CVE: %w", err)
	}

	logging.Log.Infof("Fill exploit with Exploit-DB")
	nExploitCve, err := fillWithExploitDB(dbclient.ExploitDB, r)
	if err != nil {
		return xerrors.Errorf("Failed to fill with exploit: %w", err)
	}
	logging.Log.Infof("%s: %d exploits are detected",
		r.FormatServerName(), nExploitCve)

	logging.Log.Infof("Fill metasploit module with Metasploit-DB")
	nMetasploitCve, err := fillWithMetasploit(dbclient.MetasploitDB, r)
	if err != nil {
		return xerrors.Errorf("Failed to fill with metasploit: %w", err)
	}
	logging.Log.Infof("%s: %d modules are detected",
		r.FormatServerName(), nMetasploitCve)

	logging.Log.Infof("Fill CWE with NVD")
	fillCweDict(r)

	return nil
}

// fillCvesWithNvdJvn fills CVE detail with NVD, JVN
func fillCvesWithNvdJvn(driver cvedb.DB, r *models.ScanResult) error {
	cveIDs := []string{}
	for _, v := range r.ScannedCves {
		cveIDs = append(cveIDs, v.CveID)
	}

	ds, err := CveClient.FetchCveDetails(driver, cveIDs)
	if err != nil {
		return err
	}
	for _, d := range ds {
		nvd, exploits, mitigations := models.ConvertNvdJSONToModel(d.CveID, d.NvdJSON)
		jvn := models.ConvertJvnToModel(d.CveID, d.Jvn)

		alerts := fillCertAlerts(&d)
		for cveID, vinfo := range r.ScannedCves {
			if vinfo.CveID == d.CveID {
				if vinfo.CveContents == nil {
					vinfo.CveContents = models.CveContents{}
				}
				for _, con := range []*models.CveContent{nvd, jvn} {
					if con != nil && !con.Empty() {
						vinfo.CveContents[con.Type] = *con
					}
				}
				vinfo.AlertDict = alerts
				vinfo.Exploits = append(vinfo.Exploits, exploits...)
				vinfo.Mitigations = append(vinfo.Mitigations, mitigations...)
				r.ScannedCves[cveID] = vinfo
				break
			}
		}
	}
	return nil
}

func fillCertAlerts(cvedetail *cvemodels.CveDetail) (dict models.AlertDict) {
	if cvedetail.NvdJSON != nil {
		for _, cert := range cvedetail.NvdJSON.Certs {
			dict.En = append(dict.En, models.Alert{
				URL:   cert.Link,
				Title: cert.Title,
				Team:  "us",
			})
		}
	}
	if cvedetail.Jvn != nil {
		for _, cert := range cvedetail.Jvn.Certs {
			dict.Ja = append(dict.Ja, models.Alert{
				URL:   cert.Link,
				Title: cert.Title,
				Team:  "jp",
			})
		}
	}
	return dict
}

// detectPkgsCvesWithOval fetches OVAL database
func detectPkgsCvesWithOval(driver ovaldb.DB, r *models.ScanResult) error {
	var ovalClient oval.Client
	var ovalFamily string

	switch r.Family {
	case constant.Debian, constant.Raspbian:
		ovalClient = oval.NewDebian()
		ovalFamily = constant.Debian
	case constant.Ubuntu:
		ovalClient = oval.NewUbuntu()
		ovalFamily = constant.Ubuntu
	case constant.RedHat:
		ovalClient = oval.NewRedhat()
		ovalFamily = constant.RedHat
	case constant.CentOS:
		ovalClient = oval.NewCentOS()
		//use RedHat's OVAL
		ovalFamily = constant.RedHat
	case constant.Oracle:
		ovalClient = oval.NewOracle()
		ovalFamily = constant.Oracle
	case constant.SUSEEnterpriseServer:
		// TODO other suse family
		ovalClient = oval.NewSUSE()
		ovalFamily = constant.SUSEEnterpriseServer
	case constant.Alpine:
		ovalClient = oval.NewAlpine()
		ovalFamily = constant.Alpine
	case constant.Amazon:
		ovalClient = oval.NewAmazon()
		ovalFamily = constant.Amazon
	case constant.FreeBSD, constant.Windows:
		return nil
	case constant.ServerTypePseudo:
		return nil
	default:
		if r.Family == "" {
			return xerrors.New("Probably an error occurred during scanning. Check the error message")
		}
		return xerrors.Errorf("OVAL for %s is not implemented yet", r.Family)
	}

	if !c.Conf.OvalDict.IsFetchViaHTTP() {
		if driver == nil {
			return xerrors.Errorf("You have to fetch OVAL data for %s before reporting. For details, see `https://github.com/kotakanbe/goval-dictionary#usage`", r.Family)
		}
		if err := driver.NewOvalDB(ovalFamily); err != nil {
			return xerrors.Errorf("Failed to New Oval DB. err: %w", err)
		}
	}

	logging.Log.Debugf("Check whether oval fetched: %s %s", ovalFamily, r.Release)
	ok, err := ovalClient.CheckIfOvalFetched(driver, ovalFamily, r.Release)
	if err != nil {
		return err
	}
	if !ok {
		return xerrors.Errorf("OVAL entries of %s %s are not found. Fetch OVAL before reporting. For details, see `https://github.com/kotakanbe/goval-dictionary#usage`", ovalFamily, r.Release)
	}

	_, err = ovalClient.CheckIfOvalFresh(driver, ovalFamily, r.Release)
	if err != nil {
		return err
	}

	nCVEs, err := ovalClient.FillWithOval(driver, r)
	if err != nil {
		return err
	}

	logging.Log.Infof("%s: %d CVEs are detected with OVAL", r.FormatServerName(), nCVEs)
	return nil
}

func detectPkgsCvesWithGost(driver gostdb.DB, r *models.ScanResult) error {
	nCVEs, err := gost.NewClient(r.Family).DetectUnfixed(driver, r, true)

	logging.Log.Infof("%s: %d unfixed CVEs are detected with gost",
		r.FormatServerName(), nCVEs)
	return err
}

// fillWithExploitDB fills Exploits with exploit dataabase
// https://github.com/vulsio/go-exploitdb
func fillWithExploitDB(driver exploitdb.DB, r *models.ScanResult) (nExploitCve int, err error) {
	return exploit.FillWithExploit(driver, r, &c.Conf.Exploit)
}

// fillWithMetasploit fills metasploit modules with metasploit database
// https://github.com/takuzoo3868/go-msfdb
func fillWithMetasploit(driver metasploitdb.DB, r *models.ScanResult) (nMetasploitCve int, err error) {
	return msf.FillWithMetasploit(driver, r)
}

// DetectCpeURIsCves detects CVEs of given CPE-URIs
func DetectCpeURIsCves(driver cvedb.DB, r *models.ScanResult, cpeURIs []string) error {
	nCVEs := 0
	if len(cpeURIs) != 0 && driver == nil && !c.Conf.CveDict.IsFetchViaHTTP() {
		return xerrors.Errorf("cpeURIs %s specified, but cve-dictionary DB not found. Fetch cve-dictionary before reporting. For details, see `https://github.com/kotakanbe/go-cve-dictionary#deploy-go-cve-dictionary`",
			cpeURIs)
	}

	for _, name := range cpeURIs {
		details, err := CveClient.FetchCveDetailsByCpeName(driver, name)
		if err != nil {
			return err
		}
		for _, detail := range details {
			if val, ok := r.ScannedCves[detail.CveID]; ok {
				names := val.CpeURIs
				names = util.AppendIfMissing(names, name)
				val.CpeURIs = names
				val.Confidences.AppendIfMissing(models.CpeNameMatch)
				r.ScannedCves[detail.CveID] = val
			} else {
				v := models.VulnInfo{
					CveID:       detail.CveID,
					CpeURIs:     []string{name},
					Confidences: models.Confidences{models.CpeNameMatch},
				}
				r.ScannedCves[detail.CveID] = v
				nCVEs++
			}
		}
	}
	logging.Log.Infof("%s: %d CVEs are detected with CPE", r.FormatServerName(), nCVEs)
	return nil
}

func fillCweDict(r *models.ScanResult) {
	uniqCweIDMap := map[string]bool{}
	for _, vinfo := range r.ScannedCves {
		for _, cont := range vinfo.CveContents {
			for _, id := range cont.CweIDs {
				if strings.HasPrefix(id, "CWE-") {
					id = strings.TrimPrefix(id, "CWE-")
					uniqCweIDMap[id] = true
				}
			}
		}
	}

	dict := map[string]models.CweDictEntry{}
	for id := range uniqCweIDMap {
		entry := models.CweDictEntry{}
		if e, ok := cwe.CweDictEn[id]; ok {
			if rank, ok := cwe.OwaspTopTen2017[id]; ok {
				entry.OwaspTopTen2017 = rank
			}
			if rank, ok := cwe.CweTopTwentyfive2019[id]; ok {
				entry.CweTopTwentyfive2019 = rank
			}
			if rank, ok := cwe.SansTopTwentyfive[id]; ok {
				entry.SansTopTwentyfive = rank
			}
			entry.En = &e
		} else {
			logging.Log.Debugf("CWE-ID %s is not found in English CWE Dict", id)
			entry.En = &cwe.Cwe{CweID: id}
		}

		if r.Lang == "ja" {
			if e, ok := cwe.CweDictJa[id]; ok {
				if rank, ok := cwe.OwaspTopTen2017[id]; ok {
					entry.OwaspTopTen2017 = rank
				}
				if rank, ok := cwe.CweTopTwentyfive2019[id]; ok {
					entry.CweTopTwentyfive2019 = rank
				}
				if rank, ok := cwe.SansTopTwentyfive[id]; ok {
					entry.SansTopTwentyfive = rank
				}
				entry.Ja = &e
			} else {
				logging.Log.Debugf("CWE-ID %s is not found in Japanese CWE Dict", id)
				entry.Ja = &cwe.Cwe{CweID: id}
			}
		}
		dict[id] = entry
	}
	r.CweDict = dict
	return
}
