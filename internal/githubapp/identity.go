package githubapp

// RepositoryIdentity binds a selected repository to the installation that
// grants access to it. Its fields are private so the binding cannot be changed
// after validation.
type RepositoryIdentity struct {
	installationID int64
	repositoryID   int64
}

func NewRepositoryIdentity(installationID, repositoryID int64) (RepositoryIdentity, error) {
	identity := RepositoryIdentity{installationID: installationID, repositoryID: repositoryID}
	if err := identity.validate(); err != nil {
		return RepositoryIdentity{}, err
	}
	return identity, nil
}

func (identity RepositoryIdentity) InstallationID() int64 {
	return identity.installationID
}

func (identity RepositoryIdentity) RepositoryID() int64 {
	return identity.repositoryID
}

func (identity RepositoryIdentity) validate() error {
	if identity.installationID <= 0 || identity.repositoryID <= 0 {
		return ErrInvalidIdentity
	}
	return nil
}

// RepositoryPermissions is a validated, immutable permission snapshot.
type RepositoryPermissions struct {
	contents     string
	pullRequests string
}

func ValidateRepositoryPermissions(permissions map[string]string) (RepositoryPermissions, error) {
	if permissions["contents"] != "write" || permissions["pull_requests"] != "write" {
		return RepositoryPermissions{}, ErrInsufficientPermissions
	}
	return RepositoryPermissions{contents: "write", pullRequests: "write"}, nil
}

func (permissions RepositoryPermissions) Contents() string {
	return permissions.contents
}

func (permissions RepositoryPermissions) PullRequests() string {
	return permissions.pullRequests
}
