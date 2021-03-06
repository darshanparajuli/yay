package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"

	alpm "github.com/jguer/go-alpm"
	pkgb "github.com/mikkeloscar/gopkgbuild"
)

// upgrade type describes a system upgrade.
type upgrade struct {
	Name          string
	Repository    string
	LocalVersion  string
	RemoteVersion string
}

// upSlice is a slice of Upgrades
type upSlice []upgrade

func (u upSlice) Len() int      { return len(u) }
func (u upSlice) Swap(i, j int) { u[i], u[j] = u[j], u[i] }

func (u upSlice) Less(i, j int) bool {
	iRunes := []rune(u[i].Repository)
	jRunes := []rune(u[j].Repository)

	max := len(iRunes)
	if max > len(jRunes) {
		max = len(jRunes)
	}

	for idx := 0; idx < max; idx++ {
		ir := iRunes[idx]
		jr := jRunes[idx]

		lir := unicode.ToLower(ir)
		ljr := unicode.ToLower(jr)

		if lir != ljr {
			return lir > ljr
		}

		// the lowercase runes are the same, so compare the original
		if ir != jr {
			return ir > jr
		}
	}

	return false
}

func getVersionDiff(oldVersion, newversion string) (left, right string) {
	old, errOld := pkgb.NewCompleteVersion(oldVersion)
	new, errNew := pkgb.NewCompleteVersion(newversion)

	if errOld != nil {
		left = red("Invalid Version")
	}
	if errNew != nil {
		right = red("Invalid Version")
	}

	if errOld == nil && errNew == nil {
		if old.Version == new.Version {
			left = string(old.Version) + "-" + red(string(old.Pkgrel))
			right = string(new.Version) + "-" + green(string(new.Pkgrel))
		} else {
			left = red(string(old.Version)) + "-" + string(old.Pkgrel)
			right = bold(green(string(new.Version))) + "-" + string(new.Pkgrel)
		}
	}

	return
}

// Print prints the details of the packages to upgrade.
func (u upSlice) Print(start int) {
	for k, i := range u {
		left, right := getVersionDiff(i.LocalVersion, i.RemoteVersion)

		fmt.Print(magenta(fmt.Sprintf("%2d ", len(u)+start-k-1)))
		fmt.Print(bold(colourHash(i.Repository)), "/", cyan(i.Name))

		w := 70 - len(i.Repository) - len(i.Name) + len(left)
		fmt.Printf(fmt.Sprintf("%%%ds", w),
			fmt.Sprintf("%s -> %s\n", left, right))
	}
}

// upList returns lists of packages to upgrade from each source.
func upList(dt *depTree) (aurUp upSlice, repoUp upSlice, err error) {
	local, remote, _, remoteNames, err := filterPackages()
	if err != nil {
		return
	}

	repoC := make(chan upSlice)
	aurC := make(chan upSlice)
	errC := make(chan error)

	fmt.Println(bold(cyan("::") + " Searching databases for updates..."))
	go func() {
		repoUpList, err := upRepo(local)
		errC <- err
		repoC <- repoUpList
	}()

	fmt.Println(bold(cyan("::") + " Searching AUR for updates..."))
	go func() {
		aurUpList, err := upAUR(remote, remoteNames, dt)
		errC <- err
		aurC <- aurUpList
	}()

	var i = 0
loop:
	for {
		select {
		case repoUp = <-repoC:
			i++
		case aurUp = <-aurC:
			i++
		case err := <-errC:
			if err != nil {
				fmt.Println(err)
			}
		default:
			if i == 2 {
				close(repoC)
				close(aurC)
				close(errC)
				break loop
			}
		}
	}
	return
}

func upDevel(remote []alpm.Package, packageC chan upgrade, done chan bool) {
	for vcsName, e := range savedInfo {
		if e.needsUpdate() {
			found := false
			var pkg alpm.Package
			for _, r := range remote {
				if r.Name() == vcsName {
					found = true
					pkg = r
				}
			}
			if found {
				if pkg.ShouldIgnore() {
					fmt.Print(magenta("Warning: "))
					fmt.Printf("%s ignoring package upgrade (%s => %s)\n", cyan(pkg.Name()), pkg.Version(), "git")
				} else {
					packageC <- upgrade{pkg.Name(), "devel", pkg.Version(), "latest-commit"}
				}
			} else {
				removeVCSPackage([]string{vcsName})
			}
		}
	}
	done <- true
}

// upAUR gathers foreign packages and checks if they have new versions.
// Output: Upgrade type package list.
func upAUR(remote []alpm.Package, remoteNames []string, dt *depTree) (toUpgrade upSlice, err error) {
	var routines int
	var routineDone int

	packageC := make(chan upgrade)
	done := make(chan bool)

	if config.Devel {
		routines++
		go upDevel(remote, packageC, done)
		fmt.Println(bold(cyan("::") + " Checking development packages..."))
	}

	routines++
	go func(remote []alpm.Package, remoteNames []string, dt *depTree) {
		for _, pkg := range remote {
			aurPkg, ok := dt.Aur[pkg.Name()]
			if !ok {
				continue
			}

			if (config.TimeUpdate && (int64(aurPkg.LastModified) > pkg.BuildDate().Unix())) ||
				(alpm.VerCmp(pkg.Version(), aurPkg.Version) < 0) {
				if pkg.ShouldIgnore() {
					left, right := getVersionDiff(pkg.Version(), aurPkg.Version)
					fmt.Print(magenta("Warning: "))
					fmt.Printf("%s ignoring package upgrade (%s => %s)\n", cyan(pkg.Name()), left, right)
				} else {
					packageC <- upgrade{aurPkg.Name, "aur", pkg.Version(), aurPkg.Version}
				}
			}
		}

		done <- true
	}(remote, remoteNames, dt)

	if routineDone == routines {
		err = nil
		return
	}

	for {
		select {
		case pkg := <-packageC:
			for _, w := range toUpgrade {
				if w.Name == pkg.Name {
					continue
				}
			}
			toUpgrade = append(toUpgrade, pkg)
		case <-done:
			routineDone++
			if routineDone == routines {
				err = nil
				return
			}
		}
	}
}

// upRepo gathers local packages and checks if they have new versions.
// Output: Upgrade type package list.
func upRepo(local []alpm.Package) (upSlice, error) {
	dbList, err := alpmHandle.SyncDbs()
	if err != nil {
		return nil, err
	}

	slice := upSlice{}

	for _, pkg := range local {
		newPkg := pkg.NewVersion(dbList)
		if newPkg != nil {
			if pkg.ShouldIgnore() {
				fmt.Print(magenta("Warning: "))
				fmt.Printf("%s ignoring package upgrade (%s => %s)\n", pkg.Name(), pkg.Version(), newPkg.Version())
			} else {
				slice = append(slice, upgrade{pkg.Name(), newPkg.DB().Name(), pkg.Version(), newPkg.Version()})
			}
		}
	}
	return slice, nil
}

//Contains returns whether e is present in s
func containsInt(s []int, e int) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

// RemoveIntListFromList removes all src's elements that are present in target
func removeIntListFromList(src, target []int) []int {
	max := len(target)
	for i := 0; i < max; i++ {
		if containsInt(src, target[i]) {
			target = append(target[:i], target[i+1:]...)
			max--
			i--
		}
	}
	return target
}

// upgradePkgs handles updating the cache and installing updates.
func upgradePkgs(dt *depTree) (stringSet, stringSet, error) {
	var repoNums []int
	var aurNums []int
	repoNames := make(stringSet)
	aurNames := make(stringSet)

	aurUp, repoUp, err := upList(dt)
	if err != nil {
		return repoNames, aurNames, err
	} else if len(aurUp)+len(repoUp) == 0 {
		return repoNames, aurNames, err
	}

	sort.Sort(repoUp)
	fmt.Println(bold(blue("::")), len(aurUp)+len(repoUp), bold("Packages to upgrade."))
	repoUp.Print(len(aurUp) + 1)
	aurUp.Print(1)

	if !config.NoConfirm {
		fmt.Println(bold(green(arrow + " Packages to not upgrade (eg: 1 2 3, 1-3 or ^4)")))
		fmt.Print(bold(green(arrow + " ")))
		reader := bufio.NewReader(os.Stdin)

		numberBuf, overflow, err := reader.ReadLine()
		if err != nil || overflow {
			fmt.Println(err)
			return repoNames, aurNames, err
		}

		result := strings.Fields(string(numberBuf))
		excludeAur := make([]int, 0)
		excludeRepo := make([]int, 0)
		for _, numS := range result {
			negate := numS[0] == '^'
			if negate {
				numS = numS[1:]
			}
			var numbers []int
			num, err := strconv.Atoi(numS)
			if err != nil {
				numbers, err = BuildRange(numS)
				if err != nil {
					continue
				}
			} else {
				numbers = []int{num}
			}
			for _, target := range numbers {
				if target > len(aurUp)+len(repoUp) || target <= 0 {
					continue
				} else if target <= len(aurUp) {
					target = len(aurUp) - target
					if negate {
						excludeAur = append(excludeAur, target)
					} else {
						aurNums = append(aurNums, target)
					}
				} else {
					target = len(aurUp) + len(repoUp) - target
					if negate {
						excludeRepo = append(excludeRepo, target)
					} else {
						repoNums = append(repoNums, target)
					}
				}
			}
		}
		if len(repoNums) == 0 && len(aurNums) == 0 &&
			(len(excludeRepo) > 0 || len(excludeAur) > 0) {
			if len(repoUp) > 0 {
				repoNums = BuildIntRange(0, len(repoUp)-1)
			}
			if len(aurUp) > 0 {
				aurNums = BuildIntRange(0, len(aurUp)-1)
			}
		}
		aurNums = removeIntListFromList(excludeAur, aurNums)
		repoNums = removeIntListFromList(excludeRepo, repoNums)
	}

	if len(repoUp) != 0 {
	repoloop:
		for i, k := range repoUp {
			for _, j := range repoNums {
				if j == i {
					continue repoloop
				}
			}
			repoNames.set(k.Name)
		}
	}

	if len(aurUp) != 0 {
	aurloop:
		for i, k := range aurUp {
			for _, j := range aurNums {
				if j == i {
					continue aurloop
				}
			}
			aurNames.set(k.Name)
		}
	}

	return repoNames, aurNames, err
}
