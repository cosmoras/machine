package centurylinkcloud

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/CenturyLinkLabs/clcgo"
	log "github.com/Sirupsen/logrus"
	"github.com/docker/machine/drivers"
	"github.com/docker/machine/ssh"
	"github.com/docker/machine/state"
)

const (
	statusWaitSeconds = 10
	dockerConfigDir   = "/etc/docker"
	passwordPrompt    = `Enter your CenturyLink Cloud password and press enter.
****** CAUTION: YOUR PASSWORD WILL BE VISIBLE! ******
> `
)

type Driver struct {
	MachineName    string
	CaCertPath     string
	PrivateKeyPath string
	storePath      string
	BearerToken    string
	AccountAlias   string
	ServerID       string
	Username       string
	ServerName     string
	GroupID        string
	SourceServerID string
	CPU            int
	MemoryGB       int

	// Allow Password to come in via flags while not being persisted to
	// config.json.
	Password string `json:"-"`
}

func init() {
	drivers.Register("centurylinkcloud", &drivers.RegisteredDriver{
		New:            NewDriver,
		GetCreateFlags: getCreateFlags,
	})
}

func NewDriver(machineName string, storePath string, caCert string, privateKey string) (drivers.Driver, error) {
	return &Driver{MachineName: machineName, storePath: storePath, CaCertPath: caCert, PrivateKeyPath: privateKey}, nil
}

func (d *Driver) DriverName() string {
	return "centurylinkcloud"
}

func (d *Driver) SetConfigFromFlags(flags drivers.DriverOptions) error {
	d.Username = flags.String("centurylinkcloud-username")
	d.Password = flags.String("centurylinkcloud-password")
	d.ServerName = flags.String("centurylinkcloud-server-name")
	d.GroupID = flags.String("centurylinkcloud-group-id")
	d.SourceServerID = flags.String("centurylinkcloud-source-server-id")
	d.CPU = flags.Int("centurylinkcloud-cpu")
	d.MemoryGB = flags.Int("centurylinkcloud-memory-gb")

	if d.Username == "" {
		return fmt.Errorf("centurylinkcloud driver requires the --centurylinkcloud-username option")
	}

	if d.ServerName == "" {
		return fmt.Errorf("centurylinkcloud driver requires the --centurylinkcloud-server-name option")
	}

	if d.GroupID == "" {
		return fmt.Errorf("centurylinkcloud driver requires the --centurylinkcloud-group-id option")
	}

	return nil
}

func (d *Driver) GetURL() (string, error) {
	ip, err := d.GetIP()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("tcp://%s:2376", ip), nil
}

func (d *Driver) GetIP() (string, error) {
	_, s, err := d.getServer()
	if err != nil {
		return "", err
	}

	address := publicIPFromServer(s)
	if address != "" {
		return address, nil
	}

	return "", errors.New("no IP could be found for this server")
}

func (d *Driver) GetState() (state.State, error) {
	_, s, err := d.getServer()
	if err != nil {
		return state.Error, err
	}

	if s.IsActive() {
		return state.Running, nil
	} else if s.IsPaused() {
		return state.Paused, nil
	}

	return state.Stopped, nil
}

func (d *Driver) Remove() error {
	c, s, err := d.getServer()
	if err != nil {
		return err
	}

	st, err := c.DeleteEntity(&s)
	if err != nil {
		return err
	}

	for !st.HasSucceeded() {
		time.Sleep(time.Second * statusWaitSeconds)
		if err := c.GetEntity(&st); err != nil {
			return err
		}
		log.Debugf("Deletion status: %s", st.Status)
	}

	return nil
}

func (d *Driver) Start() error {
	if err := d.doOperation(clcgo.PowerOnServer); err != nil {
		return err
	}

	return nil
}

func (d *Driver) Stop() error {
	if err := d.doOperation(clcgo.PowerOffServer); err != nil {
		return err
	}

	return nil
}

func (d *Driver) Restart() error {
	if err := d.doOperation(clcgo.RebootServer); err != nil {
		return err
	}

	return nil
}

func (d *Driver) Kill() error {
	if err := d.doOperation(clcgo.PowerOffServer); err != nil {
		return err
	}

	return nil
}

func (d *Driver) StartDocker() error {
	log.Debug("Starting Docker...")

	cmd, err := d.GetSSHCommand("sudo service docker start")
	if err != nil {
		return err
	}
	if err := cmd.Run(); err != nil {
		return err
	}

	return nil
}

func (d *Driver) StopDocker() error {
	log.Debug("Stopping Docker...")

	cmd, err := d.GetSSHCommand("sudo service docker stop")
	if err != nil {
		return err
	}
	if err := cmd.Run(); err != nil {
		return err
	}

	return nil
}

func (d *Driver) GetDockerConfigDir() string {
	return dockerConfigDir
}

func (d *Driver) Upgrade() error {
	log.Debugf("Upgrading Docker")

	cmd, err := d.GetSSHCommand("sudo apt-get update && apt-get install --upgrade lxc-docker")
	if err != nil {
		return err

	}
	if err := cmd.Run(); err != nil {
		return err

	}

	return cmd.Run()
}

func (d *Driver) GetSSHCommand(args ...string) (*exec.Cmd, error) {
	ip, err := d.GetIP()
	if err != nil {
		return nil, err
	}
	return ssh.GetSSHCommand(ip, 22, "root", d.sshKeyPath(), args...), nil
}

func (d *Driver) getClientWithPersistence() (*clcgo.Client, error) {
	c := clcgo.NewClient()
	if d.BearerToken == "" || d.AccountAlias == "" {
		if err := d.updateAPICredentials(c); err != nil {
			return nil, err
		}
	} else {
		c.APICredentials = clcgo.APICredentials{
			BearerToken:  d.BearerToken,
			AccountAlias: d.AccountAlias,
		}

		// Something to validate your BearerToken.
		err := c.GetEntity(&clcgo.DataCenters{})
		if err != nil {
			if rerr, ok := err.(clcgo.RequestError); ok && rerr.StatusCode == 401 {
				err := d.updateAPICredentials(c)
				if err != nil {
					return c, err
				}

				return c, nil
			}

			return c, err
		}
	}

	return c, nil
}

func (d *Driver) updateAPICredentials(c *clcgo.Client) error {
	var password string
	if d.Password != "" {
		password = d.Password
	} else {
		fmt.Printf(passwordPrompt)
		reader := bufio.NewReader(os.Stdin)
		input, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		password = strings.TrimSpace(input)
	}

	if err := c.GetAPICredentials(d.Username, password); err != nil {
		return err
	}
	d.AccountAlias = c.APICredentials.AccountAlias
	d.BearerToken = c.APICredentials.BearerToken
	// TODO: You need to be able to persist the config.json at this point! On
	// initial setup it will be persisted, but in two weeks when your
	// BearerToken has expired and this blows up during another call, the new
	// values are never persisted.

	return nil
}

func (d *Driver) getServer() (*clcgo.Client, clcgo.Server, error) {
	s := clcgo.Server{ID: d.ServerID}
	c, err := d.getClientWithPersistence()
	if err != nil {
		return nil, s, err
	}

	err = c.GetEntity(&s)

	if err != nil {
		if rerr, ok := err.(clcgo.RequestError); ok {
			if rerr.StatusCode == 404 {
				return nil, s, fmt.Errorf("unable to find a server with the ID '%s'", d.ServerID)
			}
		}

		return nil, s, err
	}

	return c, s, nil
}

func (d *Driver) doOperation(t clcgo.OperationType) error {
	c, s, err := d.getServer()
	if err != nil {
		return err
	}

	log.Infof("Performing '%s' operation on '%s'...", t, s.ID)
	o := clcgo.ServerOperation{Server: s, OperationType: t}
	st, err := c.SaveEntity(&o)
	if err != nil {
		return nil
	}

	for !st.HasSucceeded() {
		time.Sleep(time.Second * statusWaitSeconds)
		if err := c.GetEntity(&st); err != nil {
			return err
		}
		log.Debugf("Operation status: %s", st.Status)
	}

	return nil
}

func logAndReturnError(err error) error {
	if rerr, ok := err.(clcgo.RequestError); ok {
		for f, ms := range rerr.Errors {
			for _, m := range ms {
				log.Errorf("%v: %v", f, m)
			}
		}

		return rerr
	}

	return err
}

func publicIPFromServer(s clcgo.Server) string {
	addresses := s.Details.IPAddresses
	for _, a := range addresses {
		if a.Public != "" {
			return a.Public
		}
	}

	return ""
}

func (d *Driver) sshKeyPath() string {
	return filepath.Join(d.storePath, "id_rsa")
}
