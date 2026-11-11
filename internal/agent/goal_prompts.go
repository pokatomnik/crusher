package agent

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"
)

//go:embed templates/goal_initial.md.tpl
var goalInitialTemplate string

//go:embed templates/goal_followup.md.tpl
var goalFollowupTemplate string

var (
	goalInitial  = template.Must(template.New("goal_initial").Option("missingkey=error").Parse(goalInitialTemplate))
	goalFollowup = template.Must(template.New("goal_followup").Option("missingkey=error").Parse(goalFollowupTemplate))
)

type goalPromptData struct {
	Goal string
}

// GoalInitialPrompt renders the visible initial instruction for a goal.
func GoalInitialPrompt(text string) (string, error) {
	return renderGoalPrompt(goalInitial, text)
}

// GoalFollowupPrompt renders a hidden reminder for an active goal.
func GoalFollowupPrompt(text string) (string, error) {
	return renderGoalPrompt(goalFollowup, text)
}

func renderGoalPrompt(tmpl *template.Template, text string) (string, error) {
	var output bytes.Buffer
	if err := tmpl.Execute(&output, goalPromptData{Goal: text}); err != nil {
		return "", fmt.Errorf("rendering goal prompt: %w", err)
	}
	return output.String(), nil
}
