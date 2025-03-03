package client

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/datawire/dlib/dlog"
)

type OSSpecificConfig struct {
	Network Network `json:"network,omitempty" yaml:"network,omitempty"`
}

func GetDefaultOSSpecificConfig() OSSpecificConfig {
	return OSSpecificConfig{
		Network: Network{
			GlobalDNSSearchConfigStrategy: defaultGlobalDNSSearchConfigStrategy,
		},
	}
}

// Merge merges this instance with the non-zero values of the given argument. The argument values take priority.
func (c *OSSpecificConfig) Merge(o *OSSpecificConfig) {
	c.Network.merge(&o.Network)
}

type GSCStrategy string

const (
	// GSCAuto configure DNS search first attempting GSCPowershell and if that fails, GSCRegistry.
	GSCAuto = "auto"

	// GSCRegistry configure DNS search by setting the registry value System\CurrentControlSet\Services\Tcpip\Parameters\SearchList.
	GSCRegistry = "registry"

	// GSCPowershell configure DNS search using the powershell Set-DnsClientGlobalSetting command.
	GSCPowershell = "powershell"

	defaultGlobalDNSSearchConfigStrategy = GSCAuto
)

type Network struct {
	GlobalDNSSearchConfigStrategy GSCStrategy `json:"globalDNSSearchConfigStrategy,omitempty" yaml:"globalDNSSearchConfigStrategy,omitempty"`
}

func (n *Network) merge(o *Network) {
	if o.GlobalDNSSearchConfigStrategy != defaultGlobalDNSSearchConfigStrategy {
		n.GlobalDNSSearchConfigStrategy = o.GlobalDNSSearchConfigStrategy
	}
}

func (n Network) IsZero() bool {
	return n.GlobalDNSSearchConfigStrategy == defaultGlobalDNSSearchConfigStrategy
}

func (n *Network) UnmarshalYAML(node *yaml.Node) (err error) {
	if node.Kind != yaml.MappingNode {
		return errors.New(withLoc("network must be an object", node))
	}
	ms := node.Content
	top := len(ms)
	for i := 0; i < top; i += 2 {
		kv, err := stringKey(ms[i])
		if err != nil {
			return err
		}
		v := ms[i+1]
		switch kv {
		case "globalDNSSearchConfigStrategy":
			switch v.Value {
			case GSCAuto, GSCRegistry, GSCPowershell:
				n.GlobalDNSSearchConfigStrategy = GSCStrategy(v.Value)
			default:
				if parseContext != nil {
					dlog.Warn(parseContext, withLoc(fmt.Sprintf("invalid globalDNSSearchConfigStrategy %q. Valid values are %q, %q or %q",
						v.Value, GSCAuto, GSCRegistry, GSCPowershell), ms[i+1]))
				}
			}
		default:
			if parseContext != nil {
				dlog.Warn(parseContext, withLoc(fmt.Sprintf("unknown key %q", kv), ms[i]))
			}
		}
	}
	if n.GlobalDNSSearchConfigStrategy == "" {
		n.GlobalDNSSearchConfigStrategy = defaultGlobalDNSSearchConfigStrategy
	}
	return nil
}
