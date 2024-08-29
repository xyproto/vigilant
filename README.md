# vigilant

A utility that can be used to monitor changes in a source file in one repo and then create pull requests in another.

## Building

```bash
go build -mod=vendor
```

## Configuring

Edit `config.toml` to select which repo to monitor and which repo to create pull requests in.

Get a private GitHub token and then set the GITHUB_TOKEN environment variable, for example:

```bash
export GITHUB_TOKEN="asdfasdf" # your GitHub token with rights to read public data and create pull requests goes here
```

## Running

```bash
./vigilant
```

## General info

* Version: 0.0.1
* License: MIT
