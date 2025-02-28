package ui

import (
	"github.com/rivo/tview"
)

func CreateForm(app *tview.Application) *tview.Form {
	form := tview.NewForm().
		AddTextView("Notes", "Insert The URL And Also Choose The Queue...",
			40, 2, true, false).
		AddDropDown("Queue", []string{"Mr.", "Ms.", "Mrs.", "Dr.", "Prof."}, 0, nil).
		AddInputField("URL", "", 20, nil, nil).
		AddInputField("Name Of File", "", 20, nil, nil).
		AddButton("Submit", nil).
		AddButton("Cancel", func() {
			app.Stop()
		})
	form.SetBorder(true).SetTitle("Enter some data").SetTitleAlign(tview.AlignLeft)
	if err := app.SetRoot(form, true).EnableMouse(true).Run(); err != nil {
		panic(err)
	}
	return form
}
