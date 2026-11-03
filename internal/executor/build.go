package executor

// Limits are the settings the executors are built from.
type Limits struct {
	HTTP, JS, Command, Docker, Swarm, Kubernetes bool

	Shell string
	JSLimits

	DockerOptions
	SwarmOptions
	KubernetesOptions
}

// Build makes the executors that are turned on.
func Build(l Limits) Registry {
	r := Registry{}

	if l.HTTP {
		r[TypeHTTP] = NewHTTP()
	}

	if l.JS {
		r[TypeJS] = NewJS(l.JSLimits)
	}

	if l.Command {
		r[TypeCommand] = NewCommand(l.Shell)
	}

	if l.Docker {
		r[TypeDocker] = NewDocker(l.DockerOptions)
	}

	if l.Swarm {
		r[TypeSwarm] = NewSwarm(l.SwarmOptions)
	}

	if l.Kubernetes {
		r[TypeKubernetes] = NewKubernetes(l.KubernetesOptions)
	}

	return r
}
