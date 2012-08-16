package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/timeredbull/tsuru/cmd/term"
	"io"
	"io/ioutil"
	"net/http"
	"os"
)

type User struct{}

func readPassword(out io.Writer, password *string) error {
	io.WriteString(out, "Password: ")
	*password = term.GetPassword(os.Stdin.Fd())
	io.WriteString(out, "\n")
	if *password == "" {
		msg := "You must provide the password!\n"
		io.WriteString(out, msg)
		return errors.New(msg)
	}
	return nil
}

func (c *User) Info() *Info {
	return &Info{
		Name:    "user",
		Usage:   "user (create) [args]",
		Desc:    "manage users.",
		MinArgs: 1,
	}
}

func (c *User) Subcommands() map[string]interface{} {
	return map[string]interface{}{
		"create": &UserCreate{},
	}
}

type UserCreate struct{}

func (c *UserCreate) Info() *Info {
	return &Info{
		Name:    "create",
		Usage:   "user create username",
		Desc:    "creates user.",
		MinArgs: 1,
	}
}

func (c *UserCreate) Run(context *Context, client Doer) error {
	var password string
	email := context.Args[0]
	err := readPassword(context.Stdout, &password)
	if err != nil {
		return err
	}
	b := bytes.NewBufferString(`{"email":"` + email + `", "password":"` + password + `"}`)
	request, err := http.NewRequest("POST", GetUrl("/users"), b)
	if err != nil {
		return err
	}
	_, err = client.Do(request)
	if err != nil {
		return err
	}
	io.WriteString(context.Stdout, fmt.Sprintf(`User "%s" successfully created!`+"\n", email))
	return nil
}

type Login struct{}

func (c *Login) Run(context *Context, client Doer) error {
	var password string
	email := context.Args[0]
	err := readPassword(context.Stdout, &password)
	if err != nil {
		return err
	}
	b := bytes.NewBufferString(`{"password":"` + password + `"}`)
	request, err := http.NewRequest("POST", GetUrl("/users/"+email+"/tokens"), b)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	result, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return err
	}
	out := make(map[string]string)
	err = json.Unmarshal(result, &out)
	if err != nil {
		return err
	}
	io.WriteString(context.Stdout, "Successfully logged!\n")
	return WriteToken(out["token"])
}

func (c *Login) Info() *Info {
	return &Info{
		Name:    "login",
		Usage:   "login email",
		Desc:    "log in with your credentials.",
		MinArgs: 1,
	}
}

type Logout struct{}

func (c *Logout) Info() *Info {
	return &Info{
		Name:  "logout",
		Usage: "logout",
		Desc:  "clear local authentication credentials.",
	}
}

func (c *Logout) Run(context *Context, client Doer) error {
	tokenPath, err := joinWithUserDir(".tsuru_token")
	if err != nil {
		return err
	}
	err = filesystem().Remove(tokenPath)
	if err != nil && os.IsNotExist(err) {
		return errors.New("You're not logged in!")
	}
	io.WriteString(context.Stdout, "Successfully logout!\n")
	return nil
}

type Team struct{}

func (c *Team) Subcommands() map[string]interface{} {
	return map[string]interface{}{
		"add-user":    &TeamAddUser{},
		"remove-user": &TeamRemoveUser{},
		"create":      &TeamCreate{},
		"list":        &TeamList{},
	}
}

func (c *Team) Info() *Info {
	return &Info{
		Name:    "team",
		Usage:   "team (create|list|add-user|remove-user) [args]",
		Desc:    "manage teams.",
		MinArgs: 1,
	}
}

type TeamCreate struct{}

func (c *TeamCreate) Info() *Info {
	return &Info{
		Name:    "create",
		Usage:   "team create teamname",
		Desc:    "creates teams.",
		MinArgs: 1,
	}
}

func (c *TeamCreate) Run(context *Context, client Doer) error {
	team := context.Args[0]
	b := bytes.NewBufferString(fmt.Sprintf(`{"name":"%s"}`, team))
	request, err := http.NewRequest("POST", GetUrl("/teams"), b)
	if err != nil {
		return err
	}
	_, err = client.Do(request)
	if err != nil {
		return err
	}
	io.WriteString(context.Stdout, fmt.Sprintf(`Team "%s" successfully created!`+"\n", team))
	return nil
}

type TeamAddUser struct{}

func (c *TeamAddUser) Info() *Info {
	return &Info{
		Name:    "add-user",
		Usage:   "glb team add-user teamname username",
		Desc:    "adds user to a team",
		MinArgs: 2,
	}
}

func (c *TeamAddUser) Run(context *Context, client Doer) error {
	teamName, userName := context.Args[0], context.Args[1]
	url := GetUrl(fmt.Sprintf("/teams/%s/%s", teamName, userName))
	request, err := http.NewRequest("PUT", url, nil)
	if err != nil {
		return err
	}
	_, err = client.Do(request)
	if err != nil {
		return err
	}
	io.WriteString(context.Stdout, fmt.Sprintf(`User "%s" was added to the "%s" team`+"\n", userName, teamName))
	return nil
}

type TeamRemoveUser struct{}

func (c *TeamRemoveUser) Info() *Info {
	return &Info{
		Name:    "remove-user",
		Usage:   "glb team remove-user teamname username",
		Desc:    "removes user from a team",
		MinArgs: 2,
	}
}

func (c *TeamRemoveUser) Run(context *Context, client Doer) error {
	teamName, userName := context.Args[0], context.Args[1]
	url := GetUrl(fmt.Sprintf("/teams/%s/%s", teamName, userName))
	request, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return err
	}
	_, err = client.Do(request)
	if err != nil {
		return err
	}
	io.WriteString(context.Stdout, fmt.Sprintf(`User "%s" was removed from the "%s" team`+"\n", userName, teamName))
	return nil
}

type TeamList struct{}

func (c *TeamList) Info() *Info {
	return &Info{
		Name:    "list",
		Usage:   "team list",
		Desc:    "List all teams that you are member.",
		MinArgs: 0,
	}
}

func (c *TeamList) Run(context *Context, client Doer) error {
	request, err := http.NewRequest("GET", GetUrl("/teams"), nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(request)
	if err != nil {
		return err
	}
	if resp.StatusCode == 200 {
		defer resp.Body.Close()
		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		var teams []map[string]string
		err = json.Unmarshal(b, &teams)
		if err != nil {
			return err
		}
		io.WriteString(context.Stdout, "Teams:\n\n")
		for _, team := range teams {
			fmt.Fprintf(context.Stdout, "  - %s\n", team["name"])
		}
	}
	return nil
}
