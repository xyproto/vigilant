package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/google/go-github/v50/github"
	"github.com/spf13/viper"
	"golang.org/x/oauth2"
)

type RepoConfig struct {
	SourceRepoName string
	FilePath       string
	TargetRepoName string
	Since          string
	LastChecked    time.Time
}

type Server struct {
	githubClient *github.Client
	repoConfigs  []RepoConfig
}

func main() {
	// Load and validate configuration
	config, err := loadConfig()
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	// Set up GitHub client
	githubClient := newGitHubClient(config.GitHubToken)

	server := &Server{
		githubClient: githubClient,
		repoConfigs:  config.Repos,
	}

	// Set up signal handling for graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	GitHubToken string       `mapstructure:"GITHUB_TOKEN"`
	Repos       []RepoConfig `mapstructure:"repos"`
}

func (c *Config) validate() error {
	if c.GitHubToken == "" {
		return errors.New("GITHUB_TOKEN is required")
	}
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

func (s *Server) checkRepos() {
	for i, config := range s.repoConfigs {
		newCommits, err := s.checkRepo(config.SourceRepoName, config.FilePath, config.LastChecked)
		if err != nil {
			log.Printf("Error checking repo %s: %v", config.SourceRepoName, err)
			continue
		}

		if len(newCommits) > 0 {
			err := s.createPullRequest(context.Background(), config.TargetRepoName, config.FilePath, newCommits)
			if err != nil {
				log.Printf("Error creating pull request for repo %s: %v", config.TargetRepoName, err)
			} else {
				log.Printf("Created pull request for repo %s", config.TargetRepoName)
				s.repoConfigs[i].LastChecked = time.Now()
			}
		}
	}
}

func (s *Server) checkRepo(repoName, filePath string, lastChecked time.Time) ([]*github.RepositoryCommit, error) {
	opts := &github.CommitsListOptions{
		Path:  filePath,
		Since: lastChecked,
	}

	commits, _, err := s.githubClient.Repositories.ListCommits(context.Background(), "", repoName, opts)
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
	branchName := "xxd-update-" + time.Now().Format("20060102-150405")
	title := fmt.Sprintf("Update: Changes in %s", filePath)
	body := fmt.Sprintf("This pull request notifies that there have been changes to `%s` in the source repository.\n\n", filePath)

	for _, commit := range commits {
		body += fmt.Sprintf("- [%s](%s) - %s\n", commit.Commit.Message, commit.HTMLURL, commit.Commit.Author.Date)
	}

	// Create a new branch
	ref, _, err := s.githubClient.Git.GetRef(ctx, "", targetRepoName, "refs/heads/main")
	if err != nil {
		return err
	}

	newRef := &github.Reference{
		Ref:    github.String("refs/heads/" + branchName),
		Object: ref.Object,
	}

	_, _, err = s.githubClient.Git.CreateRef(ctx, "", targetRepoName, newRef)
	if err != nil {
		return err
	}

	// Create a notification file or update existing file
	filename := "xxd-updates.md"
	fileContent := []byte(body)
	opts := &github.RepositoryContentFileOptions{
		Message: github.String("Notify about changes to xxd.c"),
		Content: fileContent,
		Branch:  github.String(branchName),
	}

	_, _, err = s.githubClient.Repositories.CreateFile(ctx, "", targetRepoName, filename, opts)
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

	_, _, err = s.githubClient.PullRequests.Create(ctx, "", targetRepoName, newPR)
	return err
}
