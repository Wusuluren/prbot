/*
prbot is a program that finds problems to fix on GitHub
and automatically makes pull requests for them.
*/
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/go-github/github"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
)

func usage() {
	fmt.Fprintf(os.Stderr, "usage: prbot <user/repo>\n")
	flag.PrintDefaults()
}

func main() {
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 1 {
		usage()
		os.Exit(1)
	}
	parts := strings.Split(flag.Arg(0), "/")
	if len(parts) != 2 {
		usage()
		os.Exit(1)
	}
	owner, repo := parts[0], parts[1]

	tokenFile := filepath.Join(os.Getenv("HOME"), ".prbot-token")
	tokenData, err := ioutil.ReadFile(tokenFile)
	if err != nil {
		log.Fatalf("Reading auth token: %v", err)
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: string(tokenData),
	})
	tc := oauth2.NewClient(context.Background(), ts)
	gh := github.NewClient(tc)
	gh.UserAgent = "prbot/0.1"

	const branch = "master" // TODO: flag for this

	log.Printf("Resolving branch %s in github.com/%s/%s ...", branch, owner, repo)
	ref, _, err := gh.Git.GetRef(owner, repo, "refs/heads/"+branch)
	if err != nil {
		log.Fatalf("Getting ref: %v", err)
	}
	if *ref.Object.Type != "commit" {
		log.Fatalf("branch %s does not point at a commit", branch)
	}
	origCommit := *ref.Object.SHA

	log.Printf("Fetching tree for github.com/%s/%s @ %s ...", owner, repo, origCommit)
	tree, _, err := gh.Git.GetTree(owner, repo, origCommit, true /* recursive */)
	if err != nil {
		log.Fatalf("Getting tree: %v", err)
	}
	log.Printf("Original tree with %d entries: %s ...", len(tree.Entries), *tree.SHA)
	var goFiles []github.TreeEntry
	for _, te := range tree.Entries {
		if *te.Type == "blob" && strings.HasSuffix(*te.Path, ".go") {
			// Safety measure; let's stick with files under 1 MB.
			if te.Size != nil && *te.Size > 1<<20 {
				log.Printf("Warning: Skipping %s because it is too big", *te.Path)
				continue
			}
			goFiles = append(goFiles, te)
		}
	}
	log.Printf("Found %d Go source files", len(goFiles))

	// TODO: sensible rate limiting...

	var wg sync.WaitGroup
	var mu sync.Mutex
	var changes []github.TreeEntry
	add := func(base github.TreeEntry, newContents string) {
		mu.Lock()
		defer mu.Unlock()
		changes = append(changes, github.TreeEntry{
			Path:    base.Path,
			Mode:    base.Mode,
			Type:    base.Type,
			Content: github.String(newContents),
		})
	}
	for _, te := range goFiles {
		te := te
		wg.Add(1)
		go func() {
			defer wg.Done()
			abbr := fmt.Sprintf("%s %.7s", *te.Path, *te.SHA)

			in, err := rawBlob(gh, owner, repo, *te.SHA)
			if err != nil {
				log.Printf("Fetching blob (%s): %v", abbr, err)
				return
			}
			out, err := format.Source(in)
			if err != nil {
				log.Printf("Bad Go source (%s): %v", abbr, err)
				log.Printf("%s\n", in)
				return
			}
			if bytes.Equal(in, out) {
				return
			}
			log.Printf("(%s) needs gofmt'ing!", abbr)
			add(te, string(out))
		}()
	}
	wg.Wait()
	log.Printf("Found %d Go source files that need changes", len(changes))
	if len(changes) == 0 {
		return
	}

	log.Printf("Creating fork ...")
	fork, _, err := gh.Repositories.CreateFork(owner, repo, nil)
	if err != nil {
		log.Fatalf("Creating fork: %v", err)
	}
	//log.Printf("Fork: %v", fork)
	log.Printf("Fork URL: %v", *fork.HTMLURL)
	// TODO: Do we need to poll until the fork is ready?

	log.Printf("Creating new tree ...")
	newTree, _, err := gh.Git.CreateTree(*fork.Owner.Login, *fork.Name, *tree.SHA, changes)
	if err != nil {
		log.Fatalf("Creating tree: %v", err)
	}
	log.Printf("New tree: %s", *newTree.SHA)

	log.Printf("Creating commit ...")
	comm, _, err := gh.Git.CreateCommit(*fork.Owner.Login, *fork.Name, &github.Commit{
		Message: github.String("Run gofmt over Go source files."),
		Tree:    &github.Tree{SHA: newTree.SHA},
		Parents: []github.Commit{
			{SHA: github.String(origCommit)},
		},
	})
	if err != nil {
		log.Fatalf("Creating commit: %v", err)
	}
	log.Printf("Commit: %s", *comm.SHA)

	log.Printf("Creating branch ...")
	prBranch := "prbot-gofmt"
	ref, _, err = gh.Git.CreateRef(*fork.Owner.Login, *fork.Name, &github.Reference{
		Ref: github.String("refs/heads/" + prBranch),
		Object: &github.GitObject{
			Type: github.String("commit"),
			SHA:  comm.SHA,
		},
	})
	if err != nil {
		log.Fatalf("Creating branch: %v", err)
	}
	//log.Printf("Branch: %v", ref)
	log.Printf("Branch URL: %s/tree/%s", *fork.HTMLURL, prBranch)

	log.Printf("Creating pull request ...")
	pr, _, err := gh.PullRequests.Create(owner, repo, &github.NewPullRequest{
		Title: github.String("gofmt everything"),
		Head:  github.String(*fork.Owner.Login + ":" + prBranch),
		Base:  github.String(branch),
		Body:  github.String("I ran gofmt over this repository using prbot, an automated tool."),
	})
	if err != nil {
		log.Fatalf("Creating pull request: %v", err)
	}
	log.Printf("Pull request: %s", *pr.HTMLURL)
}

func rawBlob(gh *github.Client, owner, repo, sha1 string) ([]byte, error) {
	// gh.Git.GetBlob only permits getting the base64 version.
	u := fmt.Sprintf("repos/%v/%v/git/blobs/%v", owner, repo, sha1)
	req, err := gh.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3.raw")

	var buf bytes.Buffer
	if _, err = gh.Do(req, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
