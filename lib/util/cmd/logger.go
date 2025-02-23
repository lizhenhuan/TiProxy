// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"github.com/pingcap/TiProxy/lib/config"
	"github.com/pingcap/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func buildEncoder(cfg *config.Log) (zapcore.Encoder, error) {
	encfg := zap.NewProductionEncoderConfig()
	switch cfg.Encoder {
	case "tidb":
		return log.NewTextEncoder(&log.Config{})
	case "newtidb":
		fallthrough
	default:
		return NewTiDBEncoder(encfg), nil
	}
}

func buildLevel(cfg *config.Log) (zap.AtomicLevel, error) {
	return zap.ParseAtomicLevel(cfg.Level)
}

func buildSyncer(cfg *config.Log) (*AtomicWriteSyncer, error) {
	syncer := &AtomicWriteSyncer{}
	if err := syncer.Rebuild(&cfg.LogOnline); err != nil {
		return nil, err
	}
	return syncer, nil
}

func BuildLogger(cfg *config.Log) (*zap.Logger, *AtomicWriteSyncer, zap.AtomicLevel, error) {
	level, err := buildLevel(cfg)
	if err != nil {
		return nil, nil, level, err
	}
	encoder, err := buildEncoder(cfg)
	if err != nil {
		return nil, nil, level, err
	}
	syncer, err := buildSyncer(cfg)
	if err != nil {
		return nil, nil, level, err
	}
	return zap.New(zapcore.NewCore(encoder, syncer, level), zap.ErrorOutput(syncer), zap.AddStacktrace(zapcore.FatalLevel)), syncer, level, nil
}
