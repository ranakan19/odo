package validate

import (
	"reflect"

	"github.com/cli-playground/devfile-parser/pkg/devfile/parser/data/common"
	"k8s.io/klog"

	v100 "github.com/cli-playground/devfile-parser/pkg/devfile/parser/data/1.0.0"
	v200 "github.com/cli-playground/devfile-parser/pkg/devfile/parser/data/2.0.0"
	v210 "github.com/cli-playground/devfile-parser/pkg/devfile/parser/data/2.1.0"
)

// ValidateDevfileData validates whether sections of devfile are odo compatible
func ValidateDevfileData(data interface{}) error {
	var components []common.DevfileComponent

	typeData := reflect.TypeOf(data)

	if typeData == reflect.TypeOf(&v100.Devfile100{}) {
		d := data.(*v100.Devfile100)
		components = d.GetComponents()
	}

	if typeData == reflect.TypeOf(&v200.Devfile200{}) {
		d := data.(*v200.Devfile200)
		components = d.GetComponents()
	}

	if typeData == reflect.TypeOf(&v210.Devfile210{}) {
		d := data.(*v210.Devfile210)
		components = d.GetComponents()
	}

	// Validate Components
	if err := ValidateComponents(components); err != nil {
		return err
	}

	// Successful
	klog.V(4).Info("Successfully validated devfile sections")
	return nil

}
