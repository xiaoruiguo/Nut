package specification

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"gopkg.in/lxc/go-lxc.v2"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type BuilderState struct {
	Container *lxc.Container
	Env       []string
	Cwd       string
	Manifest  Manifest
}

type Spec struct {
	ID         string
	Statements []string
	State      BuilderState
}

func New(id string) *Spec {
	return &Spec{
		ID:         id,
		Statements: []string{},
	}
}

func (spec *Spec) Parse(file string) error {
	fi, err := os.Open(file)

	if err != nil {
		return err
	}
	defer fi.Close()
	scanner := bufio.NewScanner(fi)
	scanner.Split(bufio.ScanLines)
	var isComment = regexp.MustCompile(`^#`)
	var isExtendedStatement = regexp.MustCompile(`\\$`)
	previousStatement := ""
	for scanner.Scan() {
		line := scanner.Text()
		if isComment.MatchString(line) {
			log.Debug("Comment. bypassing")
			// dont process if line is comment
			continue
		} else if isExtendedStatement.MatchString(line) {
			log.Debug("Part of a multiline statement")
			// if line ends with \ then append statement
			if previousStatement != "" {
				previousStatement = previousStatement + " " + strings.TrimRight(line, "\\")
			} else {
				previousStatement = strings.TrimRight(line, "\\")
			}
		} else if strings.TrimSpace(line) == "" {
			log.Debug("Empty line. bypassing")
			// dont process if line empty
			continue
		} else {
			log.Debug("Statement completion. appending")
			// if line does not end with \ then append statement
			var statement string
			if previousStatement != "" {
				statement = previousStatement + " " + line
				previousStatement = ""
			} else {
				statement = line
			}
			spec.Statements = append(spec.Statements, statement)
		}
	}
	return nil
}

func (spec *Spec) Stop() error {
	if spec.State.Container == nil {
		return fmt.Errorf("Container is not initialized")
	}
	if !spec.State.Container.Defined() {
		return fmt.Errorf("Container is not present")
	}
	if spec.State.Container.State() == lxc.RUNNING {
		return spec.State.Container.Stop()
	}
	return nil
}
func (spec *Spec) Destroy() error {
	if spec.State.Container == nil {
		return fmt.Errorf("Container is not initialized")
	}
	if !spec.State.Container.Defined() {
		return fmt.Errorf("Container is not present")
	}
	if spec.State.Container.State() == lxc.RUNNING {
		if err := spec.State.Container.Stop(); err != nil {
			log.Errorf("Failed to stop running container. Err: %s\n", err)
			return err
		}
	}
	return spec.State.Container.Destroy()
}

func (spec *Spec) Build(volumes ...string) error {
	spec.State = BuilderState{
		Manifest: Manifest{
			Labels:       make(map[string]string),
			ExposedPorts: []uint64{},
		},
	}
	for _, statement := range spec.Statements {
		log.Infof("Proecssing:|%s|\n", statement)
		words := strings.Fields(statement)
		switch words[0] {
		case "FROM":
			if spec.State.Container != nil {
				log.Errorf("Container already built. Multiple FROM declaration?\n")
				return errors.New("Container already built. Multiple FROM declaration?")
			}
			var err error
			name := ParentName(words[1])
			for _, volume := range volumes {
				spec.State.Container, err = CloneAndStartContainer(name, spec.ID, volume)
				if err != nil {
					log.Errorf("Failed to clone container. Error: %s\n", err)
					return err
				}
			}
			manifestErr := spec.State.Manifest.Load(name)
			if manifestErr != nil {
				log.Warnf("Failed to load manifest from patent container. Error: %s\n", manifestErr)
			}
		case "RUN":
			if spec.State.Container == nil {
				log.Error("No container has been created yet. Use FROM directive")
				return errors.New("No container has been created yet. Use FROM directive")
			}
			command := words[1:len(words)]
			log.Debugf("Attempting to execute: %#v\n", command)
			if err := spec.RunCommand(command); err != nil {
				log.Errorf("Failed to run command inside container. Error: %s\n", err)
				return err
			}
		case "ENV":
			for i := 1; i < len(words); i++ {
				if strings.Contains(words[i], "=") {
					spec.State.Env = append(spec.State.Env, words[i])
					spec.State.Manifest.Env = append(spec.State.Manifest.Env, words[i])
				} else {
					spec.State.Env = append(spec.State.Env, words[i]+"="+words[i+1])
					spec.State.Manifest.Env = append(spec.State.Manifest.Env, words[i]+"="+words[i+1])
					i++
				}
			}
		case "WORKDIR":
			spec.State.Cwd = words[1]
			spec.State.Manifest.WorkDir = words[1]
		case "ADD":
			if err := spec.addFiles(words[1], words[2]); err != nil {
				return err
			}
		case "COPY":
			if err := spec.addFiles(words[1], words[2]); err != nil {
				return err
			}
		case "LABEL":
			for i := 1; i < len(words); i++ {
				if strings.Contains(words[i], "=") {
					pair := strings.Split(words[i], "=")
					spec.State.Manifest.Labels[pair[0]] = pair[1]
				} else {
					log.Fatalf("Invalid LABEL instruction. LABELS must have '=' in them")
					return errors.New("Invalid LABEL instruction. LABELS must have '=' in them")
				}
			}
		case "EXPOSE":
			for _, p := range words[1:len(words)] {
				port, err := strconv.ParseUint(p, 10, 64)
				if err != nil {
					log.Errorf("Error parsing ports in EXPOSE instruction. Err:%s\n", err)
				}
				spec.State.Manifest.ExposedPorts = append(spec.State.Manifest.ExposedPorts, port)
			}
		case "MAINTAINER":
			spec.State.Manifest.Maintainers = append(spec.State.Manifest.Maintainers, strings.Join(words[1:len(words)], " "))
		case "USER":
			spec.State.Manifest.User = words[1]
		case "VOLUME":
			// FIXME
		case "STOPSIGNAL":
			// FIXME
		case "CMD":
			if len(spec.State.Manifest.EntryPoint) == 0 {
				spec.State.Manifest.EntryPoint = words[1:]
			} else {
				log.Errorf("Entrypoint/CMD is already defined. Probably multiple declaration")
				return fmt.Errorf("Entrypoint/CMD is already defined. Probably multiple declaration")
			}
		case "ENTRYPOINT":
			if len(spec.State.Manifest.EntryPoint) == 0 {
				spec.State.Manifest.EntryPoint = words[1:]
			} else {
				log.Errorf("Entrypoint/CMD is already defined. Probably multiple declaration")
				return fmt.Errorf("Entrypoint/CMD is already defined. Probably multiple declaration")
			}
		default:
			fmt.Errorf("Unknown instruction")
		}
	}
	if err := spec.fetchArtifact(); err != nil {
		return err
	}
	return spec.writeManifest()
}

func (spec *Spec) fetchArtifact() error {
	rootfs := spec.State.Container.ConfigItem("lxc.rootfs")[0]
	for k, v := range spec.State.Manifest.Labels {
		if strings.HasPrefix(k, "nut_artifact_") {
			artifact := filepath.Base(v)
			if err := spec.RunCommand([]string{"cp", "-r", v, filepath.Join("/tmp", artifact)}); err != nil {
				log.Errorf("Failed to copy artifact to /tmp. Error: %s\n", err)
				return err
			}
			pathInContainer := filepath.Join(rootfs, "tmp", artifact)
			cmd := exec.Command("/bin/cp", "-ar", pathInContainer, artifact)
			if err := cmd.Run(); err != nil {
				log.Errorf("Failed to copy files from container to host. Error: %s\n", err)
			}
		}
	}
	return nil
}

func (spec *Spec) addFiles(src, dest string) error {
	rootfs := spec.State.Container.ConfigItem("lxc.rootfs")[0]
	absPath, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	base := filepath.Base(absPath)
	tmpContainer := filepath.Join(rootfs, "tmp", base)
	cmd := exec.Command("/bin/cp", "-ar", absPath, tmpContainer)
	log.Warnln("/bin/cp", "-ar", absPath, tmpContainer)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Errorln("Failed to copy temporary files from host to container tmp directory")
		log.Errorln("Error:", err)
		log.Errorln("Output:", out)
		return err
	}
	if err := spec.RunCommand([]string{"cp", "-r", filepath.Join("/tmp", base), dest}); err != nil {
		log.Errorf("Failed to copy temporary files within container's /tmp to target directory. Error: %s\n", err)
		return err
	}
	rmCmd := exec.Command("/bin/rm", "-rf", tmpContainer)
	if err := rmCmd.Run(); err != nil {
		log.Error("Failed to delete temporary files")
		return err
	}
	return nil
}

func (spec *Spec) writeManifest() error {
	rootfs := spec.State.Container.ConfigItem("lxc.rootfs")[0]
	manifestPath := filepath.Join(rootfs, "../manifest.yml")
	d, err := yaml.Marshal(&spec.State.Manifest)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(manifestPath, d, 0644)
}

func (spec *Spec) RunCommand(command []string) error {
	options := lxc.DefaultAttachOptions
	options.Cwd = "/root"
	options.Env = MinimalEnv
	log.Debugf("Exec environment: %#v\n", options.Env)
	rootfs := spec.State.Container.ConfigItem("lxc.rootfs")[0]
	var buffer bytes.Buffer
	buffer.WriteString("#!/bin/bash\n")
	for _, v := range spec.State.Env {
		if _, err := buffer.WriteString("export " + v + "\n"); err != nil {
			return err
		}
	}
	options.ClearEnv = true
	if spec.State.Cwd != "" {
		buffer.WriteString("cd " + spec.State.Cwd + "\n")
	}
	if spec.State.Manifest.User != "" {
		buffer.WriteString("su - " + spec.State.Manifest.User + "\n")
	}
	buffer.WriteString(strings.Join(command, " "))
	err := ioutil.WriteFile(filepath.Join(rootfs, "/tmp/dockerfile.sh"), buffer.Bytes(), 0755)
	if err != nil {
		log.Errorf("Failed to open file %s. Error: %v", err)
		return err
	}

	log.Debugf("Executing:\n %s\n", buffer.String())
	exitCode, err := spec.State.Container.RunCommandStatus([]string{"/bin/bash", "/tmp/dockerfile.sh"}, options)
	if err != nil {
		log.Errorf("Failed to execute command: '%s'. Error: %v", command, err)
		return err
	}
	if exitCode != 0 {
		log.Warnf("Failed to execute command: '%s'. Exit code: %d", strings.Join(command, " "), exitCode)
		return fmt.Errorf("Failed to execute command: '%s'. Exit code: %d", strings.Join(command, " "), exitCode)
	}
	return nil
}

func (spec *Spec) Export(file string, sudo bool) error {
	return ExportContainer(spec.State.Container.Name(), file, sudo)
}
