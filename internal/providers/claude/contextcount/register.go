package contextcount

import generic "goodkind.io/clyde/internal/contextcount"

func init() {
	if err := generic.Register(generic.CounterSourceAPI, func(deps generic.Deps) (generic.Counter, error) {
		access := deps.Access
		if access == nil {
			access = Credentials{}
		}
		return NewAPICounter(access, APICounterOptions{
			Endpoint: "",
			Client:   nil,
			Sleep:    nil,
			Clock:    deps.Clock,
		}), nil
	}); err != nil {
		panic(err)
	}
}
