/*
Copyright 2022 Hiroki Shirokura.
Copyright 2022 Keio University.
Copyright 2022 Wide Project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ipfix

import (
	"fmt"

	"github.com/wide-vsix/linux-flow-exporter/pkg/hook"
)

type OutputCollector struct {
	RemoteAddress string `yaml:"remoteAddress"`
	LocalAddress  string `yaml:"localAddress"`
}

// Hook can speficy external mechianism to make log-data updated Only one of the
// other Hook backends will be enabled.
//
// DEVELOPER_NOTE(slankdev):
// To add a new hook backend, follow hook.Command, hook.Shell in /pkg/hook. If
// you add hook.Foo to type Hook struct, please edit Hook.Valid() function and
// Hook.Execute() function at the same time.
//
// TODO(slankdev):
// performance Currently, an external program is executed for a single log data,
// but this is inefficient for a large number of log data, so batch processing
// should be used in the future.
type Hook struct {
	// Name make operator to know which hook is executed or failed.
	Name string `yaml:"name"`
	// Command is a hook to argument data using an external program like CNI.
	// It sends log data via standard input to the command it executes. Receive
	// modified log data on stdout. If the command fails, the log data is lost.
	// It respects ansible.builtin.command, but may be changed in the future.
	//
	// ## Example
	// ```
	// hooks:
	// - command: /usr/bin/cmd1
	// ```
	Command *hook.Command `yaml:"command"`
	// Shell is the backend that executes the external program. It is similar to
	// the Command hook, but it allows you to write shell scripts directly in the
	// config file, so you should use this one for simple operations. For example,
	// you can use jq to add property, resolve ifname from ifindex, add hostname,
	// and so on.
	//
	// ## Example ``` hooks: - name: test hook1
	// hooks:
	// - shell: |
	//     #!/bin/sh
	//     echo `cat` | jq --arg hostname $(hostname) '. + {hostname: $hostname}'
	// ```
	Shell *hook.Shell `yaml:"shell"`
}

func (h Hook) Valid() bool {
	cnt := 0
	if h.Command != nil {
		cnt++
	}
	if h.Shell != nil {
		cnt++
	}
	return cnt == 1
}

func (h Hook) Execute(m map[string]interface{}) (map[string]interface{}, error) {
	if !h.Valid() {
		return nil, fmt.Errorf("invalid hook")
	}
	if h.Shell != nil {
		return h.Shell.Execute(m)
	}
	if h.Command != nil {
		return h.Command.Execute(m)
	}
	return nil, fmt.Errorf("(no reach code)")
}

type OutputLog struct {
	File string `yaml:"file"`
	// Hooks are the extention for special argmentation. Multiple Hooks can be
	// set. You can change the data structure as you like by using an external
	// mechanism called flowctl agent. Hooks are arrays and are executed in order.
	// If a Hook fails, the log data will be lost.
	Hooks []Hook `yaml:"hooks"`
}

type Output struct {
	Collector *OutputCollector `yaml:"collector"`
	Log       *OutputLog       `yaml:"log"`
}

func (o Output) Valid() bool {
	return !(o.Collector != nil && o.Log != nil)
}

type Config struct {
	// MaxIpfixMessageLen Indicates the maximum size of an IPFIX message. The
	// message is divided and sent according to this value. This value is shared
	// by all collector output instances.
	MaxIpfixMessageLen int `yaml:"maxIpfixMessageLen"`
	// TimerTemplateFlushSeconds indicates the interval for sending IPFIX flow
	// template periodically. This value is shared by all collector output
	// instances.
	TimerTemplateFlushSeconds uint `yaml:"timerTemplateFlushSeconds"`
	// TimerFinishedDrainSecond indicates the interval to drain the finished Flow.
	// This interval is shared by all output instances.
	TimerFinishedDrainSeconds uint `yaml:"timerFinishedDrainSeconds"`
	// TimerForceDrainSecond specifies the interval to force a full Cache to be
	// drained for each Interface. This interval is shared by all output
	// instances.
	TimerForceDrainSeconds uint `yaml:"timerForceDrainSeconds"`
	// Output can contain multiple destinations to which the recorded flow cache
	// is transferred. IPFIX Collector, Filelog, etc. can be specified.
	Outputs   []Output `yaml:"outputs"`
	Templates []struct {
		ID       uint16 `yaml:"id"`
		Template []struct {
			Name string `yaml:"name"`
		} `yaml:"template"`
	} `yaml:"templates"`
}

type FlowFile struct {
	FlowSets []struct {
		TemplateID uint16 `yaml:"templateId"`
		Flows      []Flow `yaml:"flows"`
	} `yaml:"flowsets"`
}

func (f FlowFile) ToFlowDataMessages(config *Config,
	seqnumStart int) ([]FlowDataMessage, error) {

	flowSeq := uint32(seqnumStart)
	msgs := []FlowDataMessage{}
	for _, fs := range f.FlowSets {
		// NOTE: fragmentation is needed
		// If you send a lot of flow information, you need to split IPFIX messages
		// according to the UDP mtu size. nFlows indicates how many flow information
		// are included in one IPFIX message.
		hdrLen := 20 // ipfix-hdr(16) + flowset-hdr(4)
		flowLen, err := config.getTemplateLength(fs.TemplateID)
		if err != nil {
			return nil, err
		}
		nFlows := (int(config.MaxIpfixMessageLen) - int(hdrLen)) / int(flowLen)

		// NOTE: Assemble the IPFIX message by dividing
		// the flow list according to the nFlow value.
		flows := fs.Flows
		for len(flows) > 0 {
			var n int
			if len(flows) < nFlows {
				n = len(flows)
			} else {
				n = nFlows
			}
			msgs = append(msgs, FlowDataMessage{
				Header: Header{
					VersionNumber:  10,
					SysupTime:      0,
					SequenceNumber: flowSeq,
					SourceID:       0,
				},
				FlowSets: []FlowSet{
					{
						FlowSetID: fs.TemplateID,
						Flow:      flows[:n],
					},
				},
			})
			flowSeq += uint32(n)
			flows = flows[n:]
		}

	}
	return msgs, nil
}

type fieldTableItem struct {
	Name   string
	Value  uint16
	Length int
}

func (c Config) ToFlowTemplatesMessage() (TemplateMessage, error) {
	msg := TemplateMessage{
		Header: Header{
			VersionNumber:  10,
			SysupTime:      0,
			SequenceNumber: 0,
			SourceID:       0,
		},
	}

	for _, item := range c.Templates {
		fields := []FlowTemplateField{}
		for _, template := range item.Template {
			value, err := getIPFixFieldsValueByName(template.Name)
			if err != nil {
				return msg, err
			}
			length, err := getIPFixFieldsLengthByName(template.Name)
			if err != nil {
				return msg, err
			}

			fields = append(fields, FlowTemplateField{
				FieldType:   uint16(value),
				FieldLength: uint16(length),
			})
		}

		msg.Templates = append(msg.Templates, FlowTemplate{
			TemplateID: item.ID,
			Fields:     fields,
		})
	}
	return msg, nil
}

func getIPFixFieldsValueByName(name string) (uint16, error) {
	for _, field := range ipfixfields {
		if field.Name == name {
			return field.Value, nil
		}
	}
	return 0, fmt.Errorf("not found")
}

func getIPFixFieldsLengthByName(name string) (int, error) {
	for _, field := range ipfixfields {
		if field.Name == name {
			return field.Length, nil
		}
	}
	return 0, fmt.Errorf("not found")
}

func getTemplateFieldTypes(id uint16, config *Config) ([]uint16, error) {
	for _, template := range config.Templates {
		if template.ID == id {
			fields := []uint16{}
			for _, t := range template.Template {
				v, err := getIPFixFieldsValueByName(t.Name)
				if err != nil {
					return nil, err
				}
				fields = append(fields, uint16(v))
			}
			return fields, nil
		}
	}
	return nil, fmt.Errorf("not found")
}

func (c Config) getTemplateLength(id uint16) (int, error) {
	for _, template := range c.Templates {
		if template.ID == id {
			len := 0
			for _, item := range template.Template {
				tmpLen, err := getIPFixFieldsLengthByName(item.Name)
				if err != nil {
					return 0, err
				}
				len += tmpLen
			}
			return len, nil
		}
	}
	return 0, fmt.Errorf("not found")
}
