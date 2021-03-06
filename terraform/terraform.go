package terraform

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/capgemini/terraform-control/persistence"
	"github.com/hashicorp/go-version"
	execHelper "github.com/hashicorp/otto/helper/exec"
	"github.com/hashicorp/otto/ui"
)

var (
	tfMinVersion = version.Must(version.NewVersion("0.6.3"))
)

// Terraform wraps `terraform` execution into an easy-to-use API
type Terraform struct {
	// Path is the path to Terraform itself. If empty, "terraform"
	// will be used and looked up via the PATH var.
	Path string

	// Dir is the working directory where all Terraform commands are executed
	Dir string

	// Ui, if given, will be used to stream output from the Terraform commands.
	// If this is nil, then the output will be logged but won't be visible
	// to the user.
	Ui ui.Ui

	// Variables is a list of variables to pass to Terraform.
	Variables map[string]string

	Directory persistence.Backend

	StateId string
}

// Execute executes a raw Terraform command
func (t *Terraform) Execute(commandRaw ...string) error {
	command := make([]string, 1, len(commandRaw)*2)
	command[0] = commandRaw[0]
	commandArgs := commandRaw[1:]

	// Determine if we need to skip var flags or not.
	varSkip := false
	varSkip = command[0] == "get"

	// If we have variables, create the var file
	if !varSkip && len(t.Variables) > 0 {
		varfile, err := t.varfile()
		if err != nil {
			return err
		}
		if execHelper.ShouldCleanup() {
			defer os.Remove(varfile)
		}

		// Append the varfile onto our command.
		command = append(command, "-var-file", varfile)

		// Log some of the vars we're using
		for k, _ := range t.Variables {
			log.Printf("[DEBUG] setting TF var: %s", k)
		}
	}

	// Determine if we need to skip state flags or not. This is just
	// hardcoded for now.
	stateSkip := false
	stateSkip = command[0] == "get"

	// Output needs state but not state-out; more hard-coding
	//TODO: this was false!!!
	stateOutSkip := false
	//stateOutSkip = command[0] == "output"

	// If we care about state, then setup the state directory and
	// load it up.
	var stateDir, statePath string
	if !stateSkip && t.StateId != "" && t.Directory != nil {

		stateDir = t.Dir
		stateOldPath := filepath.Join(stateDir, "state.old")
		statePath = filepath.Join(stateDir, "state")

		// Load the state from the directory
		data, err := t.Directory.GetBlob(t.StateId)
		if err != nil {
			return fmt.Errorf("Error loading Terraform state: %s", err)
		}
		if data == nil && command[0] == "destroy" {
			// Destroy we can just execute, we don't need state
			return nil
		}
		if data != nil {
			err = data.WriteToFile(stateOldPath)
			data.Close()
		}
		if err != nil {
			return fmt.Errorf("Error writing Terraform state: %s", err)
		}

		// Append the state to the args
		command = append(command, "-state", stateOldPath)

		if command[0] == "apply" {
			command = append(command, "-state-out", statePath)
		}
		// if !stateOutSkip {
		// 	command = append(command, "-state-out", statePath)
		// }
	}

	// Append all the final args
	command = append(command, commandArgs...)
	// TODO: We need to redo the way we stream and store terraform output
	// once we do it we should show color as well
	command = append(command, "-no-color")

	// Build the command to execute
	log.Printf("[DEBUG] executing terraform: %v", command)
	path := "terraform"
	if t.Path != "" {
		path = t.Path
	}

	// Get terraform modules
	cmd := exec.Command(path, "get")
	cmd.Dir = t.Dir
	err := execHelper.Run(t.Ui, cmd)
	if err != nil {
		err = fmt.Errorf("Error running Terraform for getting modules: %s", err)
	}

	buildModulesScript := "if ls -1 .terraform/modules/*/Makefile >/dev/null 2>&1; then for dir in .terraform/modules/*/Makefile; do make -C $(/usr/bin/dirname $dir); done fi"

	// Hacky hacky to build terraform modules.
	cmd = exec.Command("/bin/bash", "-c", buildModulesScript)
	cmd.Dir = t.Dir
	err = execHelper.Run(t.Ui, cmd)
	if err != nil {
		err = fmt.Errorf("Error running Terraform for getting modules: %s", err)
	}

	cmd = exec.Command(path, command...)
	cmd.Dir = t.Dir

	// Start the Terraform command. If there is an error we just store
	// the error but can't exit yet because we have to store partial
	// state if there is any.
	err = execHelper.Run(t.Ui, cmd)
	if err != nil {
		err = fmt.Errorf("Error running Terraform: %s", err)
	}

	// Save the state file if we have it.
	if command[0] == "apply" && t.StateId != "" && t.Directory != nil && statePath != "" && !stateOutSkip {
		f, ferr := os.Open(statePath)
		if ferr != nil {
			return fmt.Errorf(
				"Error reading Terraform state for saving: %s", ferr)
		}

		// Store the state
		derr := t.Directory.PutBlob(t.StateId, &persistence.BlobData{
			Data: f,
		})
		// Always close the file
		f.Close()

		// If we couldn't save the data, then note the error. This is a
		// _really_ bad error to get since there isn't a good way to
		// recover. For now, we just copy the state to the pwd and note
		// the user.
		if derr != nil {
			// TODO: copy state

			err = fmt.Errorf(
				"Failed to save Terraform state: %s\n\n"+
					"This means that Otto was unable to store the state of your infrastructure.\n"+
					"At this time, Otto doesn't support gracefully recovering from this\n"+
					"scenario. The state should be in the path below. Please ask the\n"+
					"community for assistance.",
				derr)
		}
	}

	return err
}

func (t *Terraform) varfile() (string, error) {
	f, err := ioutil.TempFile("", "tf-control-vars")
	if err != nil {
		return "", err
	}

	err = json.NewEncoder(f).Encode(t.Variables)
	f.Close()
	if err != nil {
		os.Remove(f.Name())
	}

	return f.Name(), err
}
