package main

import (
	"container/list"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path"
	"strings"
	"time"

	"github.com/ovh/cds/sdk"
)

// ProcessActionVariables replaces all placeholders inside action recursively using
// - parent parameters
// - action build arguments
// - Secrets from project, application and environment
//
// This function should be called ONLY from worker
func processActionVariables(a *sdk.Action, parent *sdk.Action, ab sdk.ActionBuild, secrets []sdk.Variable) error {
	// replaces placeholder in parameters with ActionBuild variables
	// replaces placeholder in parameters with Parent params
	for i := range a.Parameters {
		keepReplacing := true
		for keepReplacing {
			t := a.Parameters[i].Value

			if parent != nil {
				for _, p := range parent.Parameters {
					log.Printf("Replacing %s (%s) with %s (%s)\n", a.Parameters[i].Name, a.Parameters[i].Value, p.Name, p.Value)
					a.Parameters[i].Value = strings.Replace(a.Parameters[i].Value, "{{."+p.Name+"}}", p.Value, -1)
				}
			}

			for _, p := range ab.Args {
				a.Parameters[i].Value = strings.Replace(a.Parameters[i].Value, "{{."+p.Name+"}}", p.Value, -1)
			}

			for _, p := range secrets {
				a.Parameters[i].Value = strings.Replace(a.Parameters[i].Value, "{{."+p.Name+"}}", p.Value, -1)

			}

			// If parameters wasn't updated, consider it done
			if a.Parameters[i].Value == t {
				keepReplacing = false
			}
		}
	}

	// replaces placeholder in all children recursively
	for i := range a.Actions {
		err := processActionVariables(&a.Actions[i], a, ab, secrets)
		if err != nil {
			return nil
		}
	}

	return nil
}

func startAction(a *sdk.Action, actionBuild sdk.ActionBuild) sdk.Result {

	// Process action build arguments
	for _, abp := range actionBuild.Args {

		// Process build variable for root action
		for j := range a.Parameters {
			if abp.Name == a.Parameters[j].Name {
				a.Parameters[j].Value = abp.Value
			}
		}
	}

	return runAction(a, actionBuild)
}

func replaceBuildVariablesPlaceholder(a *sdk.Action) {
	for i := range a.Parameters {
		for _, v := range buildVariables {
			a.Parameters[i].Value = strings.Replace(a.Parameters[i].Value,
				"{{.cds.build."+v.Name+"}}", v.Value, -1)

		}
	}
}

func runAction(a *sdk.Action, actionBuild sdk.ActionBuild) sdk.Result {
	r := sdk.Result{
		Status:  sdk.StatusFail,
		BuildID: actionBuild.ID,
	}

	// Replace build variable placeholder that may have been added by last step
	replaceBuildVariablesPlaceholder(a)

	if a.Type == sdk.BuiltinAction {
		return runBuiltin(a, actionBuild)
	}
	if a.Type == sdk.PluginAction {
		return runPlugin(a, actionBuild)
	}

	// Nothing to do, success !
	if len(a.Actions) == 0 {
		r.Status = sdk.StatusSuccess
		return r
	}

	var nbDisabledChildren int

	finalActions := []sdk.Action{}
	var doNotRunChildrenAnymore bool
	for i, child := range a.Actions {
		if !child.Enabled {
			childName := fmt.Sprintf("%s/%s-%d", a.Name, child.Name, i+1)
			sendLog(actionBuild.ID, childName, fmt.Sprintf("%s: Step %s is disabled\n", name, childName))
			nbDisabledChildren++
			continue
		}

		if child.Final {
			finalActions = append(finalActions, child)
		} else {
			if !doNotRunChildrenAnymore {
				childName := fmt.Sprintf("%s/%s-%d", a.Name, child.Name, i+1)
				log.Printf("Running %s\n", childName)
				sendLog(actionBuild.ID, childName, fmt.Sprintf("%s: Starting step %s...\n", name, childName))
				r = startAction(&child, actionBuild)
				sendLog(actionBuild.ID, childName, fmt.Sprintf("%s: Step %s finished (status: %s)\n", name, childName, r.Status))
				if r.Status != sdk.StatusSuccess {
					log.Printf("Stopping %s at step %s", a.Name, childName)
					doNotRunChildrenAnymore = true
				}
			}
		}
	}

	//If all steps are disabled, set action status to disabled
	if nbDisabledChildren >= (len(a.Actions) - len(finalActions)) {
		r.Status = sdk.StatusDisabled
	}

	for i, child := range finalActions {
		childName := fmt.Sprintf("%s/%s-%d", a.Name, child.Name, i+1)
		log.Printf("Running final action : %s\n", childName)
		sendLog(actionBuild.ID, childName, fmt.Sprintf("%s: Starting final step %s...\n", name, childName))
		finalActionResult := startAction(&child, actionBuild)
		//If action is success or disabled we consider final action status
		if r.Status == sdk.StatusSuccess || r.Status == sdk.StatusDisabled {
			r = finalActionResult
		}
		if finalActionResult.Status != sdk.StatusSuccess {
			log.Printf("Stoping %s at final step %s", a.Name, childName)
			return r
		}
		sendLog(actionBuild.ID, childName, fmt.Sprintf("%s: Final step %s finished (status: %s)\n", name, childName, finalActionResult.Status))
	}

	return r
}

var logsecrets []sdk.Variable

func sendLog(buildid int64, step string, value string) error {
	for i := range logsecrets {
		if len(logsecrets[i].Value) >= 6 {
			value = strings.Replace(value, logsecrets[i].Value, "**"+logsecrets[i].Name+"**", -1)
		}
	}

	l := sdk.NewLog(buildid, step, value)
	logChan <- *l
	return nil
}

func logger(inputChan chan sdk.Log) {
	llist := list.New()

	for {
		select {
		case l, ok := <-inputChan:
			if ok {
				llist.PushBack(l)
			}
			break
		case <-time.After(1 * time.Second):
			var logs []sdk.Log

			// While list is not empty
			for llist.Len() > 0 {
				// get older log line
				l := llist.Front().Value.(sdk.Log)
				llist.Remove(llist.Front())

				// then count how many lines are exactly the same
				count := 1
				for llist.Len() > 0 {
					n := llist.Front().Value.(sdk.Log)
					if n.Value != l.Value {
						break
					}
					count++
					llist.Remove(llist.Front())
				}

				// and if count > 1, then add it at the beginning of the log
				if count > 1 {
					l.Value = fmt.Sprintf("[x%d] %s", count, l.Value)
				}
				// and append to the logs batch
				l.Value = strings.Trim(strings.Replace(l.Value, "\n", " ", -1), " \t\n") + "\n"
				logs = append(logs, l)
			}

			if len(logs) == 0 {
				continue
			}

			// Buffer log list is empty, sending batch to API
			data, err := json.Marshal(logs)
			if err != nil {
				fmt.Printf("Error: cannot marshal logs: %s\n", err)
				continue
			}

			path := fmt.Sprintf("/build/%d/log", logs[0].ActionBuildID)
			_, _, err = sdk.Request("POST", path, data)
			if err != nil {
				fmt.Printf("error: cannot send logs: %s\n", err)
				continue
			}

			break
		}
	}
}

// creates a working directory in $HOME/PROJECT/APP/PIP/BN
func setupBuildDirectory(wd string) error {

	err := os.MkdirAll(wd, 0755)
	if err != nil {
		return err
	}

	err = os.Chdir(wd)
	if err != nil {
		return err
	}

	err = os.Setenv("HOME", wd)
	if err != nil {
		return err
	}

	return nil
}

// remove the buildDirectory created by setupBuildDirectory
func teardownBuildDirectory(wd string) error {

	err := os.RemoveAll(wd)
	if err != nil {
		return err
	}

	return nil
}

func generateWorkingDirectory() (string, error) {
	size := 16
	bs := make([]byte, size)
	_, err := rand.Read(bs)
	if err != nil {
		return "", err
	}
	str := hex.EncodeToString(bs)
	token := []byte(str)[0:size]

	return string(token), nil
}

func workingDirectory(basedir string, ab sdk.ActionBuild) string {

	gen, _ := generateWorkingDirectory()

	dir := path.Join(basedir,
		fmt.Sprintf("%d", ab.PipelineID),
		fmt.Sprintf("%d", ab.PipelineActionID),
		fmt.Sprintf("%d", ab.BuildNumber),
		gen)

	return dir
}

func run(a sdk.Action, ab sdk.ActionBuild, secrets []sdk.Variable) sdk.Result {
	// REPLACE ALL VARIABLE EVEN SECRETS HERE
	err := processActionVariables(&a, nil, ab, secrets)
	if err != nil {
		log.Printf("takeActionBuildHandler> Cannot process action %s parameters: %s\n", a.Name, err)
		return sdk.Result{Status: sdk.StatusFail}
	}

	// Add secrets as string in ActionBuild.Args
	// So they can be used by plugins
	for _, s := range secrets {
		p := sdk.Parameter{
			Type:  sdk.StringParameter,
			Name:  s.Name,
			Value: s.Value,
		}
		ab.Args = append(ab.Args, p)
	}

	// If action is not done within 1 hour, KILL IT WITH FIRE
	doneChan := make(chan bool)
	go func() {
		for {
			select {
			case <-doneChan:
				return
			case <-time.After(12 * time.Hour):
				sendLog(ab.ID, "SYSTEM", fmt.Sprintf("Error: Action %s running for 12 hour on worker %s, aborting", a.Name, name))
				path := fmt.Sprintf("/queue/%d/result", ab.ID)
				body, _ := json.Marshal(sdk.Result{Status: sdk.StatusFail})
				sdk.Request("POST", path, body)
				time.Sleep(5 * time.Second)
				os.Exit(1)
			}
		}
	}()

	// Setup working directory
	wd := workingDirectory(basedir, ab)
	err = setupBuildDirectory(wd)
	if err != nil {
		sendLog(ab.ID, "SYSTEM", fmt.Sprintf("Error: cannot setup working directory (%s)", err))
		time.Sleep(5 * time.Second)
		return sdk.Result{Status: sdk.StatusFail}
	}

	// Setup user ssh keys
	err = setupSSHKey(secrets, path.Join(wd, ".ssh"))
	if err != nil {
		sendLog(ab.ID, "SYSTEM", fmt.Sprintf("Error: cannot setup ssh key (%s)", err))
		time.Sleep(5 * time.Second)
		return sdk.Result{Status: sdk.StatusFail}
	}

	logsecrets = secrets
	res := startAction(&a, ab)
	close(doneChan)
	logsecrets = nil

	err = teardownBuildDirectory(wd)
	if err != nil {
		fmt.Printf("Cannot remove build directory: %s\n", err)
	}

	sendLog(ab.ID, "SYSTEM", "Done.\n")
	fmt.Printf("Run> Done.\n")
	return res
}
