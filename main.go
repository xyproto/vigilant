package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
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
	SourceRepoName        string `mapstructure:"source_repo_name"`
	FilePath              string `mapstructure:"file_path"`
	TargetRepoName        string `mapstructure:"target_repo_name"`
	PullRequestBaseBranch string `mapstructure:"pull_request_base_branch"`
}

type Config struct {
	PollInterval int          `mapstructure:"poll_interval"`
	Repos        []RepoConfig `mapstructure:"repos"`
}

type Server struct {
	githubClient *github.Client
	repoConfigs  []RepoConfig
	mu           sync.Mutex
	cachePath    string
	pollInterval time.Duration
	lastChecked  time.Time
}

func main() {
	// Determine cache directory based on OS
	cacheDir := getCacheDir()

	// Load and validate configuration
	config, err := loadConfig(cacheDir)
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
		cachePath:    filepath.Join(cacheDir, "since.timestamp"),
		pollInterval: time.Duration(config.PollInterval) * time.Minute,
	}

	// Initialize the since.timestamp file if it doesn't exist
	server.initSinceFile()

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

func getCacheDir() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Failed to get home directory: %v", err)
	}

	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(homeDir, "Library", "Caches", "vigilant")
	case "linux":
		return filepath.Join(homeDir, ".cache", "vigilant")
	default:
		log.Fatalf("Unsupported OS: %s", runtime.GOOS)
		return ""
	}
}

func loadConfig(cacheDir string) (*Config, error) {
	viper.SetConfigType("toml")

	// Define the search paths for the config file
	configPaths := []string{
		"/etc/vigilant",
		filepath.Join(os.Getenv("HOME"), ".config/vigilant"),
		".",
	}

	var configPath string
	for _, path := range configPaths {
		viper.AddConfigPath(path)
		if err := viper.ReadInConfig(); err == nil {
			configPath = viper.ConfigFileUsed()
			break
		}
	}

	if configPath == "" {
		return nil, fmt.Errorf("could not find config.toml in any of the expected locations")
	}

	var config Config
	if err := viper.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("unable to decode config into struct: %w", err)
	}

	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	// Create cache directory if it doesn't exist
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	return &config, nil
}

func (s *Server) initSinceFile() {
	// Check if since.timestamp exists
	if _, err := os.Stat(s.cachePath); os.IsNotExist(err) {
		// Create default since.timestamp with current time
		log.Println("Creating default since.timestamp...")
		s.lastChecked = time.Now()
		s.updateSinceInCache()
	} else {
		// Load existing since.timestamp
		s.loadSinceValue()
	}
}

func (s *Server) loadSinceValue() {
	data, err := os.ReadFile(s.cachePath)
	if err != nil {
		log.Printf("Could not read since.timestamp: %v. Assuming first run.", err)
		s.lastChecked = time.Now()
		return
	}

	s.lastChecked, err = time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		log.Printf("Error parsing since.timestamp: %v. Using current time.", err)
		s.lastChecked = time.Now()
	}
}

func (c *Config) validate() error {
	if c.PollInterval <= 0 {
		return errors.New("poll_interval must be greater than zero")
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
		if repo.PullRequestBaseBranch == "" {
			return errors.New("each repo configuration must have a PullRequestBaseBranch")
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
	ticker := time.NewTicker(s.pollInterval)
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
			wg.Wait()
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
	for _, config := range s.repoConfigs {
		log.Printf("Checking repo %s for changes in %s...", config.SourceRepoName, config.FilePath)
		newCommits, err := s.checkRepo(config.SourceRepoName, config.FilePath, s.lastChecked)
		if err != nil {
			log.Printf("Error checking repo %s: %v", config.SourceRepoName, err)
			continue
		}

		if len(newCommits) > 0 {
			log.Printf("Found %d new commit(s) in %s. Creating pull request...", len(newCommits), config.FilePath)
			err := s.createPullRequest(context.Background(), config.TargetRepoName, config.FilePath, config.PullRequestBaseBranch, newCommits)
			if err != nil {
				log.Printf("Error creating pull request for repo %s: %v", config.TargetRepoName, err)
			} else {
				log.Printf("Created pull request for repo %s", config.TargetRepoName)
				s.lastChecked = time.Now()
				s.updateSinceInCache()
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

func (s *Server) createPullRequest(ctx context.Context, targetRepoName, filePath, baseBranch string, commits []*github.RepositoryCommit) error {
	owner, repo := parseRepoName(targetRepoName)

	branchName := fmt.Sprintf("%s-update-%s", strings.ReplaceAll(filePath, "/", "-"), time.Now().Format("20060102-150405"))
	title := fmt.Sprintf("Update: Changes in %s", filePath)
	body := fmt.Sprintf("This pull request notifies that there have been changes to `%s` in the source repository.\n\n", filePath)

	for _, commit := range commits {
		body += fmt.Sprintf("- [%s](%s) - %s\n", *commit.Commit.Message, *commit.HTMLURL, commit.Commit.Author.Date.Format(time.RFC1123))
	}

	// Create a new branch
	ref, _, err := s.githubClient.Git.GetRef(ctx, owner, repo, fmt.Sprintf("refs/heads/%s", baseBranch))
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
		Base:  github.String(baseBranch),
		Body:  github.String(body),
	}

	_, _, err = s.githubClient.PullRequests.Create(ctx, owner, repo, newPR)
	return err
}

func (s *Server) updateSinceInCache() {
	// Write the lastChecked time to since.timestamp
	data := s.lastChecked.Format(time.RFC3339)
	if err := os.WriteFile(s.cachePath, []byte(data), 0644); err != nil {
		log.Printf("Error writing updated since.timestamp to file: %v", err)
	} else {
		log.Printf("Updated last checked time in since.timestamp: %s", data)
	}
}

func parseRepoName(fullRepoName string) (owner, repo string) {
	parts := strings.Split(fullRepoName, "/")
	if len(parts) != 2 {
		log.Fatalf("Invalid repository name: %s", fullRepoName)
	}
	return parts[0], parts[1]
}
