package src

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"

	"code.google.com/p/rog-go/parallel"
	"sourcegraph.com/sourcegraph/go-sourcegraph/router"
	"sourcegraph.com/sourcegraph/go-sourcegraph/sourcegraph"
	"sourcegraph.com/sourcegraph/rwvfs"
	"sourcegraph.com/sourcegraph/srclib/buildstore"
)

func init() {
	_, err := CLI.AddCommand("pull",
		"fetch remote build data to local dir",
		"Fetch remote build data (from Sourcegraph.com) for the current repository to the local .srclib-cache directory.",
		&pullCmd,
	)
	if err != nil {
		log.Fatal(err)
	}

	_, err = CLI.AddCommand("push",
		"upload local build data to remote",
		"Upload local build data (in .srclib-cache) for the current repository to Sourcegraph.com.",
		&pushCmd,
	)
	if err != nil {
		log.Fatal(err)
	}
}

type PullCmd struct {
	List bool `short:"l" long:"list" description:"only list files that exist on remote; don't fetch"`
	URLs bool `long:"urls" description:"show URLs to build data files"`
}

var pullCmd PullCmd

func (c *PullCmd) Execute(args []string) error {
	repo, err := OpenRepo(".")
	if err != nil {
		return err
	}

	if GlobalOpt.Verbose {
		log.Printf("Listing remote build files for repository %q commit %q...", repo.URI, repo.CommitID)
	}

	rr := sourcegraph.RepoRevSpec{
		RepoSpec: sourcegraph.RepoSpec{URI: repo.URI()},
		Rev:      repo.CommitID,
		CommitID: repo.CommitID,
	}
	remoteFiles, resp, err := apiclient.BuildData.List(rr, nil)
	if err != nil {
		if hresp, ok := resp.(*sourcegraph.HTTPResponse); hresp != nil && ok && hresp.StatusCode == http.StatusNotFound {
			log.Println("No remote build files found.")
			return nil
		} else {
			log.Fatal(err)
		}
	}

	if c.List {
		log.Printf("# Remote build files for repository %q commit %s:", repo.URI, repo.CommitID)
		for _, file := range remoteFiles {
			fmt.Printf("%7s   %s   %s\n", bytesString(uint64(file.Size)), file.ModTime, file.Path)
			if c.URLs {
				bdspec := sourcegraph.BuildDataFileSpec{RepoRev: rr, Path: file.Path}
				u := router.URITo(router.RepoBuildDataEntry, router.MapToArray(bdspec.RouteVars())...)
				u.Host = apiclient.BaseURL.Host
				u.Scheme = apiclient.BaseURL.Scheme
				fmt.Println(" @", u)
			}
		}
		return nil
	}

	repoStore, err := buildstore.NewRepositoryStore(repo.RootDir)
	if err != nil {
		return err
	}

	par := parallel.NewRun(8)
	for _, file_ := range remoteFiles {
		file := file_
		par.Do(func() error {
			return fetchFile(repoStore, repo.URI(), file)
		})
	}
	return par.Wait()
}

func fetchFile(repoStore *buildstore.RepositoryStore, repoURI string, fi *buildstore.BuildDataFileInfo) error {
	path := repoStore.FilePath(fi.CommitID, fi.Path)

	fileSpec := sourcegraph.BuildDataFileSpec{
		RepoRev: sourcegraph.RepoRevSpec{
			RepoSpec: sourcegraph.RepoSpec{URI: repoURI},
			Rev:      fi.CommitID,
			CommitID: fi.CommitID,
		},
		Path: fi.Path,
	}

	kb := float64(fi.Size) / 1024
	if GlobalOpt.Verbose {
		log.Printf("Fetching %s (%.1fkb)", path, kb)
	}

	// Use uncached API client because the .srclib-cache already
	// caches it, and we want to be able to stream large files.
	apiclientUncached := sourcegraph.NewClient(nil)
	apiclientUncached.BaseURL = apiclient.BaseURL
	remoteFile, _, err := apiclientUncached.BuildData.Get(fileSpec)
	if err != nil {
		return err
	}
	defer remoteFile.Close()

	if GlobalOpt.Verbose {
		log.Printf("Fetched %s (%.1fkb)", path, kb)
	}

	err = rwvfs.MkdirAll(repoStore, filepath.Dir(path))
	if err != nil {
		return err
	}

	f, err := repoStore.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(f, remoteFile); err != nil {
		return err
	}

	if GlobalOpt.Verbose {
		log.Printf("Saved %s", path)
	}

	return nil
}

type PushCmd struct {
	List bool `short:"l" long:"list" description:"only list files that exist on remote; don't fetch"`
}

var pushCmd PushCmd

func (c *PushCmd) Execute(args []string) error {
	repo, err := OpenRepo(".")
	if err != nil {
		return err
	}

	if GlobalOpt.Verbose {
		log.Printf("Listing local build files for repository %q commit %q...", repo.URI, repo.CommitID)
	}

	repoStore, err := buildstore.NewRepositoryStore(repo.RootDir)
	if err != nil {
		log.Fatal(err)
	}

	localFiles, err := repoStore.AllDataFiles()
	if err != nil {
		log.Fatal(err)
	}

	if c.List {
		log.Printf("# Local build files for repository %q commit %s:", repo.URI, repo.CommitID)
		for _, file := range localFiles {
			fmt.Println(file.Path)
		}
		return nil
	}

	par := parallel.NewRun(8)
	for _, file_ := range localFiles {
		file := file_
		par.Do(func() error {
			return uploadFile(repoStore, file, repo.URI())
		})
	}
	return par.Wait()
}

func uploadFile(repoStore *buildstore.RepositoryStore, file *buildstore.BuildDataFileInfo, repoURI string) error {
	path := repoStore.FilePath(file.CommitID, file.Path)

	fileSpec := sourcegraph.BuildDataFileSpec{
		RepoRev: sourcegraph.RepoRevSpec{
			RepoSpec: sourcegraph.RepoSpec{URI: repoURI},
			Rev:      file.CommitID,
			CommitID: file.CommitID,
		},
		Path: file.Path,
	}

	fi, err := repoStore.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		if GlobalOpt.Verbose {
			log.Printf("upload: skipping nonexistent file %s", path)
		}
		return nil
	}

	kb := float64(fi.Size()) / 1024
	if GlobalOpt.Verbose {
		log.Printf("Uploading %s (%.1fkb)", path, kb)
	}

	f, err := repoStore.Open(path)
	if err != nil {
		return err
	}

	_, err = apiclient.BuildData.Upload(fileSpec, f)
	if err != nil {
		return err
	}

	if GlobalOpt.Verbose {
		log.Printf("Uploaded %s (%.1fkb)", path, kb)
	}

	return nil
}
