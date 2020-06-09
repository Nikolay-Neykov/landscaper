// Copyright 2020 Copyright (c) 2020 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package installations

import (
	"context"
	"fmt"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	landscaperv1alpha1 "github.com/gardener/landscaper/pkg/apis/core/v1alpha1"
	landscaperv1alpha1helper "github.com/gardener/landscaper/pkg/apis/core/v1alpha1/helper"
)

// EnsureSubInstallations ensures that all referenced definitions are mapped to a installation.
func (a *actuator) EnsureSubInstallations(ctx context.Context, inst *landscaperv1alpha1.ComponentInstallation, def *landscaperv1alpha1.ComponentDefinition) error {
	cond := landscaperv1alpha1helper.GetOrInitCondition(inst.Status.Conditions, landscaperv1alpha1.EnsureSubInstallationsCondition)

	subInstallations, err := a.getSubInstallations(ctx, inst)
	if err != nil {
		return err
	}

	// need to check if we are allowed to update subinstallation
	// - we are not allowed if any subresource is in deletion
	// - we are not allowed to update if any subinstallation is progressing
	for _, subInstallations := range subInstallations {
		if subInstallations.DeletionTimestamp != nil {
			a.log.V(7).Info("not eligible for update due to deletion of subinstallation", "name", subInstallations.Name)
			return a.updateInstallationStatus(ctx, inst, landscaperv1alpha1.ComponentPhaseProgressing, cond)
		}

		if subInstallations.Status.Phase == landscaperv1alpha1.ComponentPhaseProgressing {
			a.log.V(7).Info("not eligible for update due to running subinstallation", "name", subInstallations.Name)
			return a.updateInstallationStatus(ctx, inst, landscaperv1alpha1.ComponentPhaseProgressing, cond)
		}
	}

	// delete removed subreferences
	err, deleted := a.cleanupOrphanedSubInstallations(ctx, def, inst, subInstallations)
	if err != nil {
		return err
	}
	if deleted {
		return nil
	}

	for _, subDef := range def.DefinitionReferences {
		// skip if the subInstallation already exists
		subInst, ok := subInstallations[subDef.Name]
		if ok {
			if !installationNeedsUpdate(subDef, subInst) {
				continue
			}
		}

		subInst, err := a.createOrUpdateNewInstallation(ctx, inst, def, subDef, subInst)
		if err != nil {
			return errors.Wrapf(err, "unable to create installation for %s", subDef.Name)
		}
	}

	cond = landscaperv1alpha1helper.UpdatedCondition(cond, landscaperv1alpha1.ConditionTrue,
		"InstallationsInstalled", "All Installations are successfully installed")
	return a.updateInstallationStatus(ctx, inst, inst.Status.Phase, cond)
}

func (a *actuator) getSubInstallations(ctx context.Context, inst *landscaperv1alpha1.ComponentInstallation) (map[string]*landscaperv1alpha1.ComponentInstallation, error) {
	var (
		cond             = landscaperv1alpha1helper.GetOrInitCondition(inst.Status.Conditions, landscaperv1alpha1.EnsureSubInstallationsCondition)
		subInstallations = map[string]*landscaperv1alpha1.ComponentInstallation{}

		// track all found subinstallation to track if some installations were deleted
		updatedSubInstallationStates = make([]landscaperv1alpha1.NamedObjectReference, 0)
	)

	for _, installationRef := range inst.Status.InstallationReferences {
		subInst := &landscaperv1alpha1.ComponentInstallation{}
		if err := a.c.Get(ctx, installationRef.Reference.NamespacedName(), subInst); err != nil {
			if !apierrors.IsNotFound(err) {
				a.log.Error(err, "unable to get installation", "object", installationRef.Reference)
				cond = landscaperv1alpha1helper.UpdatedCondition(cond, landscaperv1alpha1.ConditionFalse,
					"InstallationNotFound", fmt.Sprintf("Sub Installation %s not available", installationRef.Reference.Name))
				_ = a.updateInstallationStatus(ctx, inst, landscaperv1alpha1.ComponentPhaseProgressing, cond)
				return nil, errors.Wrapf(err, "unable to get installation %v", installationRef.Reference)
			}
			continue
		}
		subInstallations[installationRef.Name] = subInst
		updatedSubInstallationStates = append(updatedSubInstallationStates, installationRef)
	}

	// update the sub components if installations changed
	if len(updatedSubInstallationStates) != len(inst.Status.InstallationReferences) {
		if err := a.c.Status().Update(ctx, inst); err != nil {
			return nil, errors.Wrapf(err, "unable to update sub installation status")
		}
	}
	return subInstallations, nil
}

func (a *actuator) cleanupOrphanedSubInstallations(ctx context.Context, def *landscaperv1alpha1.ComponentDefinition, inst *landscaperv1alpha1.ComponentInstallation, subInstallations map[string]*landscaperv1alpha1.ComponentInstallation) (error, bool) {
	var (
		cond    = landscaperv1alpha1helper.GetOrInitCondition(inst.Status.Conditions, landscaperv1alpha1.EnsureSubInstallationsCondition)
		deleted = false
	)

	for defName, subInst := range subInstallations {
		if _, ok := getDefinitionReference(def, defName); ok {
			continue
		}

		// delete installation
		a.log.V(5).Info("delete orphaned installation", "name", subInst.Name)
		if err := a.c.Delete(ctx, subInst); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			cond = landscaperv1alpha1helper.UpdatedCondition(cond, landscaperv1alpha1.ConditionFalse,
				"InstallationNotDeleted", fmt.Sprintf("Sub Installation %s cannot be deleted", subInst.Name))
			_ = a.updateInstallationStatus(ctx, inst, landscaperv1alpha1.ComponentPhaseFailed, cond)
			return err, deleted
		}
		deleted = true
	}
	return nil, deleted
}

func (a *actuator) createOrUpdateNewInstallation(ctx context.Context, inst *landscaperv1alpha1.ComponentInstallation, def *landscaperv1alpha1.ComponentDefinition, subDefRef landscaperv1alpha1.DefinitionReference, subInst *landscaperv1alpha1.ComponentInstallation) (*landscaperv1alpha1.ComponentInstallation, error) {
	cond := landscaperv1alpha1helper.GetOrInitCondition(inst.Status.Conditions, landscaperv1alpha1.EnsureSubInstallationsCondition)

	if subInst == nil {
		subInst = &landscaperv1alpha1.ComponentInstallation{}
		subInst.Name = fmt.Sprintf("%s-%s-", def.Name, subDefRef.Name)
		subInst.Namespace = inst.Namespace
	}

	subDef, err := a.registry.GetDefinitionByRef(subDefRef.Reference)
	if err != nil {
		cond = landscaperv1alpha1helper.UpdatedCondition(cond, landscaperv1alpha1.ConditionFalse,
			"ComponentDefinitionNotFound",
			fmt.Sprintf("ComponentDefinition %s for %s cannot be found", subDefRef.Reference, subDefRef.Name))
		_ = a.updateInstallationStatus(ctx, inst, landscaperv1alpha1.ComponentPhaseFailed, cond)
		return nil, errors.Wrapf(err, "unable to get definition %s for %s", subDefRef.Reference, subDefRef.Name)
	}

	_, err = controllerruntime.CreateOrUpdate(ctx, a.c, subInst, func() error {
		subInst.Labels = map[string]string{landscaperv1alpha1.EncompassedByLabel: inst.Name}
		if err := controllerutil.SetOwnerReference(inst, subInst, a.scheme); err != nil {
			return errors.Wrapf(err, "unable to set owner reference")
		}
		subInst.Spec = landscaperv1alpha1.ComponentInstallationSpec{
			DefinitionRef: subDefRef.Reference,
			Imports:       subDefRef.Imports,
			Exports:       subDefRef.Exports,
		}

		AddDefaultMappings(subInst, subDef)
		return nil
	})
	if err != nil {
		cond = landscaperv1alpha1helper.UpdatedCondition(cond, landscaperv1alpha1.ConditionFalse,
			"InstallationCreatingFailed",
			fmt.Sprintf("Sub Installation %s cannot be created", subDefRef.Name))
		_ = a.updateInstallationStatus(ctx, inst, landscaperv1alpha1.ComponentPhaseFailed, cond)
		return nil, errors.Wrapf(err, "unable to create installation for %s", subDefRef.Name)
	}

	// add newly created installation to state
	inst.Status.InstallationReferences = append(inst.Status.InstallationReferences, landscaperv1alpha1helper.NewInstallationReferenceState(subDefRef.Name, subInst))
	if err := a.c.Status().Update(ctx, inst); err != nil {
		return nil, errors.Wrapf(err, "unable to add new installation for %s to state", subDefRef.Name)
	}

	return subInst, nil
}

// installationNeedsUpdate check if a definition reference has been updated
func installationNeedsUpdate(def landscaperv1alpha1.DefinitionReference, inst *landscaperv1alpha1.ComponentInstallation) bool {
	if def.Reference != inst.Spec.DefinitionRef {
		return true
	}

	for _, mapping := range def.Imports {
		if !hasMappingOfImports(mapping, inst.Spec.Imports) {
			return true
		}
	}

	for _, mapping := range def.Exports {
		if !hasMappingOfExports(mapping, inst.Spec.Exports) {
			return true
		}
	}

	if len(inst.Spec.Imports) != len(def.Imports) {
		return true
	}

	if len(inst.Spec.Exports) != len(def.Exports) {
		return true
	}

	return false
}

func hasMappingOfImports(search landscaperv1alpha1.DefinitionImportMapping, mappings []landscaperv1alpha1.DefinitionImportMapping) bool {
	for _, mapping := range mappings {
		if mapping.To == search.To && mapping.From == search.From {
			return true
		}
	}
	return false
}

func hasMappingOfExports(search landscaperv1alpha1.DefinitionExportMapping, mappings []landscaperv1alpha1.DefinitionExportMapping) bool {
	for _, mapping := range mappings {
		if mapping.To == search.To && mapping.From == search.From {
			return true
		}
	}
	return false
}

// getDefinitionReference returns the definition reference by name
func getDefinitionReference(def *landscaperv1alpha1.ComponentDefinition, name string) (landscaperv1alpha1.DefinitionReference, bool) {
	for _, ref := range def.DefinitionReferences {
		if ref.Name == name {
			return ref, true
		}
	}
	return landscaperv1alpha1.DefinitionReference{}, false
}