package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/go-github/v50/github"
	"github.com/spf13/viper"
	"github.com/xyproto/env/v2"
	"golang.org/x/oauth2"
)

type RepoConfig struct {
	SourceRepoName string `mapstructure:"source_repo_name"`
	FilePath       string `mapstructure:"file_path"`
	TargetRepoName string `mapstructure:"target_repo_name"`
	Since          string `mapstructure:"since"`
	LastChecked    time.Time
}

type Server struct {
	githubClient *github.Client
	repoConfigs  []RepoConfig
	mu           sync.Mutex // Protects manual check trigger
}

func main() {
	// Load and validate configuration
	config, err := loadConfig()
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	// Read GITHUB_TOKEN from the environment
	githubToken := env.Str("GITHUB_TOKEN", "")
	if githubToken == "" {
		log.Fatal("GITHUB_TOKEN environment variable is required")
	}

	// Set up GitHub client
	githubClient := newGitHubClient(githubToken)

	server := &Server{
		githubClient: githubClient,
		repoConfigs:  config.Repos,
	}

	// Set up signal handling for manual checks and graceful shutdown
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGUSR1, syscall.SIGTERM, syscall.SIGINT)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	go func() {
		for sig := range sigs {
			switch sig {
			case syscall.SIGUSR1:
				log.Println("Received SIGUSR1, manually triggering repository check...")
				server.triggerManualCheck()
			case syscall.SIGTERM, syscall.SIGINT:
				log.Println("Received termination signal, shutting down...")
				stop()
				return
			}
		}
	}()

	// Run the server
	server.Run(ctx)
}

func loadConfig() (*Config, error) {
	viper.SetConfigName("config")
	viper.SetConfigType("toml")
	viper.AddConfigPath(".")
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	var config Config
	if err := viper.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("unable to decode config into struct: %w", err)
	}

	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	// Parse the 'since' date into time.Time format
	for i := range config.Repos {
		parsedSince, err := time.Parse("2006-01-02", config.Repos[i].Since)
		if err != nil {
			return nil, fmt.Errorf("invalid date format for 'since': %w", err)
		}
		config.Repos[i].LastChecked = parsedSince
	}

	return &config, nil
}

type Config struct {
	Repos []RepoConfig `mapstructure:"repos"`
}

func (c *Config) validate() error {
	if len(c.Repos) == 0 {
		return errors.New("at least one repo configuration is required")
	}
	for _, repo := range c.Repos {
		if repo.SourceRepoName == "" {
			return errors.New("each repo configuration must have a SourceRepoName")
		}
		if repo.FilePath == "" {
			return errors.New("each repo configuration must have a FilePath")
		}
		if repo.TargetRepoName == "" {
			return errors.New("each repo configuration must have a TargetRepoName")
		}
		if repo.Since == "" {
			return errors.New("each repo configuration must have a Since date")
		}
	}
	return nil
}

func newGitHubClient(token string) *github.Client {
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)
	return github.NewClient(tc)
}

func (s *Server) Run(ctx context.Context) {
	log.Println("Starting server...")
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	var wg sync.WaitGroup

	for {
		select {
		case <-ticker.C:
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.checkRepos()
			}()
		case <-ctx.Done():
			log.Println("Shutting down server...")
			wg.Wait() // Wait for all goroutines to finish
			log.Println("Server stopped.")
			return
		}
	}
}

func (s *Server) triggerManualCheck() {
	s.mu.Lock()
	defer s.mu.Unlock()

	go s.checkRepos()
}

func (s *Server) checkRepos() {
	log.Println("Checking repositories for updates...")
	for i, config := range s.repoConfigs {
		log.Printf("Checking repo %s for changes in %s...", config.SourceRepoName, config.FilePath)
		newCommits, err := s.checkRepo(config.SourceRepoName, config.FilePath, config.LastChecked)
		if err != nil {
			log.Printf("Error checking repo %s: %v", config.SourceRepoName, err)
			continue
		}

		if len(newCommits) > 0 {
			log.Printf("Found %d new commit(s) in %s. Creating pull request...", len(newCommits), config.FilePath)
			err := s.createPullRequest(context.Background(), config.TargetRepoName, config.FilePath, newCommits)
			if err != nil {
				log.Printf("Error creating pull request for repo %s: %v", config.TargetRepoName, err)
			} else {
				log.Printf("Created pull request for repo %s", config.TargetRepoName)
				s.repoConfigs[i].LastChecked = time.Now()
			}
		} else {
			log.Printf("No new commits found for %s in repo %s.", config.FilePath, config.SourceRepoName)
		}
	}
}

func (s *Server) checkRepo(repoName, filePath string, lastChecked time.Time) ([]*github.RepositoryCommit, error) {
	owner, repo := parseRepoName(repoName)

	opts := &github.CommitsListOptions{
		Path:  filePath,
		Since: lastChecked,
	}

	commits, _, err := s.githubClient.Repositories.ListCommits(context.Background(), owner, repo, opts)
	if err != nil {
		return nil, err
	}

	var newCommits []*github.RepositoryCommit
	for _, commit := range commits {
		if commit.Commit.Author.Date.After(lastChecked) {
			newCommits = append(newCommits, commit)
		}
	}

	return newCommits, nil
}

func (s *Server) createPullRequest(ctx context.Context, targetRepoName, filePath string, commits []*github.RepositoryCommit) error {
	owner, repo := parseRepoName(targetRepoName)

	branchName := fmt.Sprintf("%s-update-%s", strings.ReplaceAll(filePath, "/", "-"), time.Now().Format("20060102-150405"))
	title := fmt.Sprintf("Update: Changes in %s", filePath)
	body := fmt.Sprintf("This pull request notifies that there have been changes to `%s` in the source repository.\n\n", filePath)

	for _, commit := range commits {
		body += fmt.Sprintf("- [%s](%s) - %s\n", *commit.Commit.Message, *commit.HTMLURL, commit.Commit.Author.Date.Format(time.RFC1123))
	}

	// Create a new branch
	ref, _, err := s.githubClient.Git.GetRef(ctx, owner, repo, "refs/heads/main")
	if err != nil {
		return err
	}

	newRef := &github.Reference{
		Ref:    github.String("refs/heads/" + branchName),
		Object: ref.Object,
	}

	_, _, err = s.githubClient.Git.CreateRef(ctx, owner, repo, newRef)
	if err != nil {
		return err
	}

	// Create a notification file or update existing file
	filename := strings.ReplaceAll(filePath, "/", "-") + "-updates.md"
	fileContent := []byte(body)
	opts := &github.RepositoryContentFileOptions{
		Message: github.String(fmt.Sprintf("Notify about changes to %s", filePath)),
		Content: fileContent,
		Branch:  github.String(branchName),
	}

	_, _, err = s.githubClient.Repositories.CreateFile(ctx, owner, repo, filename, opts)
	if err != nil {
		return err
	}

	// Create a pull request
	newPR := &github.NewPullRequest{
		Title: github.String(title),
		Head:  github.String(branchName),
		Base:  github.String("main"),
		Body:  github.String(body),
	}

	_, _, err = s.githubClient.PullRequests.Create(ctx, owner, repo, newPR)
	return err
}

func parseRepoName(fullRepoName string) (owner, repo string) {
	parts := strings.Split(fullRepoName, "/")
	if len(parts) != 2 {
		log.Fatalf("Invalid repository name: %s", fullRepoName)
	}
	return parts[0], parts[1]
}
