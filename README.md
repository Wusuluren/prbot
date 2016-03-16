# prbot

This is a program that automatically generates pull requests.

It is limited in scope right now: it looks for Go files that need gofmt'ing,
and makes a pull request to fix that up.

## Authentication

Visit https://github.com/settings/tokens and create a personal access token,
while logged in to GitHub as the user as whom you wish to make pull requests.
Make sure it has the `repo:public_repo` scope.

Store the token in `$HOME/.prbot-token` and chmod 600 that file.
