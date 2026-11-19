package procedure

// builtins are the part of Robot's BuiltIn library the subset keeps. Each
// does what Robot's does, so that a procedure reads the same to someone who
// knows Robot; anything else of BuiltIn is refused by name (refusedKeywords).
func builtins() []*Keyword {
	return []*Keyword{
		{
			Name:   "Log",
			Doc:    "Records a message as a step. It does nothing else.",
			Params: []Param{AnyP("message"), Opt(Text("level"), "INFO")},
			Run:    func(*Ctx, Args) (Value, error) { return Value{}, nil },
		},
		{
			Name:   "Fail",
			Doc:    "Fails the test with the message; [Teardown] runs.",
			Params: []Param{Opt(Text("message"), "Failed")},
			Run: func(_ *Ctx, a Args) (Value, error) {
				return Value{}, Failf(Fail, "%s", a["message"].Str)
			},
		},
		{
			Name:     "Set Variable",
			Doc:      "Returns its value, for assigning: ${x}=    Set Variable    12V. Variables are local to the test or keyword.",
			Params:   []Param{AnyP("value")},
			ReturnOf: func(a map[string]Static) Static { return a["value"] },
			Run:      func(_ *Ctx, a Args) (Value, error) { return a["value"], nil },
		},
		{
			Name: "Should Be True",
			Doc: "Fails unless the condition is true. The condition is an expression of the condition profile, " +
				"over variables ($x or ${x}) and channels.",
			Params: []Param{Cond("condition"), Opt(Text("msg"), "")},
			Run: func(c *Ctx, a Args) (Value, error) {
				cond := c.Condition("condition")
				ok, err := cond.Eval()
				if err != nil {
					return Value{}, err
				}
				if !ok {
					msg := a["msg"].Str
					if msg == "" {
						msg = "'" + cond.Source() + "' should be true"
					}
					return Value{}, Failf(Fail, "%s", msg)
				}
				return Value{}, nil
			},
		},
	}
}
