package directory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// Fake is a directory read from a JSON file, for development and tests. It
// accepts any non-empty password, which is why config refuses it on an https
// deployment.
type Fake struct {
	mu    sync.Mutex
	users []User
}

// fakeUser is the file format: managers by username rather than DN, because
// that is what a person editing the file thinks in.
type fakeUser struct {
	Username   string `json:"username"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	Department string `json:"department"`
	Manager    string `json:"manager"`
	Admin      bool   `json:"admin"`
}

func fakeDN(username string) string { return "CN=" + username + ",OU=Fake" }

func LoadFake(path string) (*Fake, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fake directory: %w", err)
	}
	var in []fakeUser
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, fmt.Errorf("fake directory %s: %w", path, err)
	}
	f := &Fake{}
	for _, u := range in {
		f.Add(User{
			Username: NormalizeUsername(u.Username), Name: u.Name, Email: u.Email,
			Department: u.Department, Admin: u.Admin,
			ManagerDN: func() string {
				if u.Manager == "" {
					return ""
				}
				return fakeDN(NormalizeUsername(u.Manager))
			}(),
		})
	}
	return f, nil
}

// Add inserts or replaces a user (by username). ManagerDN may be given as a
// bare username for convenience in tests.
func (f *Fake) Add(u User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u.Username = NormalizeUsername(u.Username)
	u.DN = fakeDN(u.Username)
	if u.ID == "" {
		u.ID = "fake-" + u.Username
	}
	for i := range f.users {
		if f.users[i].Username == u.Username {
			f.users[i] = u
			return
		}
	}
	f.users = append(f.users, u)
}

// Remove drops a user, as disabling an account in AD would.
func (f *Fake) Remove(username string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	username = NormalizeUsername(username)
	for i := range f.users {
		if f.users[i].Username == username {
			f.users = append(f.users[:i], f.users[i+1:]...)
			return
		}
	}
}

// DN returns the DN the fake assigns a username, for building ManagerDN.
func (f *Fake) DN(username string) string { return fakeDN(NormalizeUsername(username)) }

func (f *Fake) Authenticate(_ context.Context, username, password string) (User, error) {
	if password == "" {
		return User{}, ErrInvalidCredentials
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	username = NormalizeUsername(username)
	for _, u := range f.users {
		if u.Username == username {
			return u, nil
		}
	}
	return User{}, ErrInvalidCredentials
}

func (f *Fake) AllUsers(context.Context) ([]User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]User(nil), f.users...), nil
}
