// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package app

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v2"

	"github.com/elastic/beats/x-pack/agent/pkg/agent/errors"
	"github.com/elastic/beats/x-pack/agent/pkg/core/plugin/authority"
	"github.com/elastic/beats/x-pack/agent/pkg/core/plugin/process"
	"github.com/elastic/beats/x-pack/agent/pkg/core/plugin/state"
	"github.com/elastic/beats/x-pack/agent/pkg/core/remoteconfig"
	"github.com/elastic/beats/x-pack/agent/pkg/core/remoteconfig/grpc"
)

const (
	configurationFlag     = "-c"
	configFileTempl       = "%s.yml" // providing beat id
	configFilePermissions = 0644     // writable only by owner
)

// Start starts the application with a specified config.
func (a *Application) Start(cfg map[string]interface{}) (err error) {
	defer func() {
		if err != nil {
			// inject App metadata
			err = errors.New(err, errors.M(errors.MetaKeyAppName, a.name), errors.M(errors.MetaKeyAppName, a.id))
		}
	}()
	a.appLock.Lock()
	defer a.appLock.Unlock()

	if a.state.Status == state.Running {
		return nil
	}

	defer func() {
		if err != nil {
			// reportError()
			a.state.Status = state.Stopped
		}
	}()

	if err := a.monitor.Prepare(a.uid, a.gid); err != nil {
		return err
	}

	spec, err := a.spec.Spec(a.downloadConfig)
	if err != nil {
		return errors.New(err, errors.TypeFilesystem)
	}

	if err := a.configureByFile(&spec, cfg); err != nil {
		return errors.New(err, errors.TypeApplication)
	}

	// TODO: provider -> client
	ca, err := generateCA(spec.Configurable)
	if err != nil {
		return errors.New(err, errors.TypeSecurity)
	}
	processCreds, err := generateConfigurable(spec.Configurable, ca)
	if err != nil {
		return errors.New(err, errors.TypeSecurity)
	}

	if a.limiter != nil {
		a.limiter.Add()
	}

	spec.Args = a.monitor.EnrichArgs(spec.Args)

	// specify beat name to avoid data lock conflicts
	// as for https://github.com/elastic/beats/pull/14030 more than one instance
	// of the beat with same data path fails to start
	spec.Args = injectDataPath(spec.Args, a.pipelineID, a.id)

	a.state.ProcessInfo, err = process.Start(
		a.logger,
		spec.BinaryPath,
		a.processConfig,
		a.uid,
		a.gid,
		processCreds,
		spec.Args...)
	if err != nil {
		return err
	}

	a.grpcClient, err = generateClient(spec.Configurable, a.state.ProcessInfo.Address, a.clientFactory, ca)
	if err != nil {
		return errors.New(err, errors.TypeSecurity)
	}
	a.state.Status = state.Running

	// setup watcher
	a.watch(a.state.ProcessInfo.Process, cfg)

	return nil
}

func injectDataPath(args []string, pipelineID, id string) []string {
	wd := ""
	if w, err := os.Getwd(); err == nil {
		wd = w
	}

	dataPath := filepath.Join(wd, "data", pipelineID, id)
	return append(args, "-E", "path.data="+dataPath)
}

func generateCA(configurable string) (*authority.CertificateAuthority, error) {
	if !isGrpcConfigurable(configurable) {
		return nil, nil
	}

	ca, err := authority.NewCA()
	if err != nil {
		return nil, errors.New(err, "app.Start", errors.TypeSecurity)
	}

	return ca, nil
}

func generateConfigurable(configurable string, ca *authority.CertificateAuthority) (*process.Creds, error) {
	var processCreds *process.Creds
	var err error

	if isGrpcConfigurable(configurable) {
		processCreds, err = getProcessCredentials(configurable, ca)
		if err != nil {
			return nil, errors.New(err, errors.TypeSecurity)
		}
	}

	return processCreds, nil
}

func generateClient(configurable, address string, factory remoteconfig.ConnectionCreator, ca *authority.CertificateAuthority) (remoteconfig.Client, error) {
	var grpcClient remoteconfig.Client

	if isGrpcConfigurable(configurable) {
		connectionProvider, err := getConnectionProvider(configurable, ca, address)
		if err != nil {
			return nil, errors.New(err, errors.TypeNetwork)
		}

		grpcClient, err = factory.NewConnection(connectionProvider)
		if err != nil {
			return nil, errors.New(err, "creating connection", errors.TypeNetwork)
		}
	}

	return grpcClient, nil
}

func getConnectionProvider(configurable string, ca *authority.CertificateAuthority, address string) (*grpc.ConnectionProvider, error) {
	if !isGrpcConfigurable(configurable) {
		return nil, nil
	}

	clientPair, err := ca.GeneratePair()
	if err != nil {
		return nil, errors.New(err, errors.TypeNetwork)
	}

	return grpc.NewConnectionProvider(address, ca.Crt(), clientPair.Key, clientPair.Crt), nil
}

func isGrpcConfigurable(configurable string) bool {
	return configurable == ConfigurableGrpc
}

func (a *Application) configureByFile(spec *ProcessSpec, config map[string]interface{}) error {
	// check if configured by file
	if spec.Configurable != ConfigurableFile {
		return nil
	}

	// save yaml as filebeat_id.yml
	filename := fmt.Sprintf(configFileTempl, a.id)
	filePath, err := filepath.Abs(filepath.Join(a.downloadConfig.InstallPath, filename))
	if err != nil {
		return errors.New(err, errors.TypeFilesystem)
	}

	f, err := os.OpenFile(filePath, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, configFilePermissions)
	if err != nil {
		return errors.New(err, errors.TypeFilesystem)
	}
	defer f.Close()

	// change owner
	if err := os.Chown(filePath, a.uid, a.gid); err != nil {
		return err
	}

	encoder := yaml.NewEncoder(f)
	if err := encoder.Encode(config); err != nil {
		return errors.New(err, errors.TypeFilesystem)
	}
	defer encoder.Close()

	// update args
	return updateSpecConfig(spec, filePath)
}

func updateSpecConfig(spec *ProcessSpec, configPath string) error {
	// check if config is already provided
	configIndex := -1
	for i, v := range spec.Args {
		if v == configurationFlag {
			configIndex = i
			break
		}
	}

	if configIndex != -1 {
		// -c provided
		if len(spec.Args) == configIndex+1 {
			// -c is last argument, appending
			spec.Args = append(spec.Args, configPath)
		}
		spec.Args[configIndex+1] = configPath
		return nil
	}

	spec.Args = append(spec.Args, configurationFlag, configPath)
	return nil
}

func getProcessCredentials(configurable string, ca *authority.CertificateAuthority) (*process.Creds, error) {
	var processCreds *process.Creds

	if isGrpcConfigurable(configurable) {
		// processPK and Cert serves as a server credentials
		processPair, err := ca.GeneratePair()
		if err != nil {
			return nil, errors.New(err, "failed to generate credentials")
		}

		processCreds = &process.Creds{
			CaCert: ca.Crt(),
			PK:     processPair.Key,
			Cert:   processPair.Crt,
		}
	}

	return processCreds, nil
}
