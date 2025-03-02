package usage

import (
	"context"
	"sort"

	"github.com/infracost/infracost/internal/schema"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type SyncResult struct {
	ResourceCount    int
	EstimationCount  int
	EstimationErrors map[string]error
}

type MergeResourceUsagesOpts struct {
	OverrideValueType bool
}

func SyncUsageData(usageFile *UsageFile, projects []*schema.Project) (*SyncResult, error) {
	referenceFile, err := LoadReferenceFile()
	if err != nil {
		return nil, err
	}
	referenceFile.SetDefaultValues()

	// TODO: update this when we properly support multiple projects in usage
	resources := make([]*schema.Resource, 0)
	for _, project := range projects {
		resources = append(resources, project.Resources...)
	}

	syncResult := syncResourceUsages(usageFile, resources, referenceFile)

	return syncResult, nil
}

func syncResourceUsages(usageFile *UsageFile, resources []*schema.Resource, referenceFile *ReferenceFile) *SyncResult {
	syncResult := &SyncResult{
		EstimationErrors: make(map[string]error),
	}

	existingResourceUsagesMap := resourceUsagesMap(usageFile.ResourceUsages)
	resourceUsages := make([]*ResourceUsage, 0, len(resources))

	// Track the existing order so we can keep these at the top
	existingOrder := make([]string, 0, len(usageFile.ResourceUsages))
	for _, resourceUsage := range usageFile.ResourceUsages {
		existingOrder = append(existingOrder, resourceUsage.Name)
	}

	for _, resource := range resources {
		resourceUsage := &ResourceUsage{
			Name: resource.Name,
		}

		// Merge the usage schema from the reference usage file
		refResourceUsage := referenceFile.FindMatchingResourceUsage(resource.Name)
		if refResourceUsage != nil {
			mergeResourceUsages(resourceUsage, refResourceUsage, MergeResourceUsagesOpts{})
		}

		// Merge the usage schema from the resource struct
		// We want to override the value type from the usage schema since we can't always tell from the YAML
		// what the value type should be, e.g. user might add an int value for a float attribute.
		mergeResourceUsages(resourceUsage, &ResourceUsage{
			Name:  resource.Name,
			Items: resource.UsageSchema,
		}, MergeResourceUsagesOpts{OverrideValueType: true})

		// Merge any existing resource usage
		existingResourceUsage := existingResourceUsagesMap[resource.Name]
		if existingResourceUsage != nil {
			mergeResourceUsages(resourceUsage, existingResourceUsage, MergeResourceUsagesOpts{})
		}

		syncResult.ResourceCount++
		if resource.EstimateUsage != nil {
			syncResult.EstimationCount++

			resourceUsageMap := resourceUsage.Map()
			err := resource.EstimateUsage(context.TODO(), resourceUsageMap)
			if err != nil {
				syncResult.EstimationErrors[resource.Name] = err
				log.Warnf("Error estimating usage for resource %s: %v", resource.Name, err)
			}

			// Merge in the estimated usage
			estimatedUsageData := schema.NewUsageData(resource.Name, schema.ParseAttributes(resourceUsageMap))
			mergeResourceUsageWithUsageData(resourceUsage, estimatedUsageData)
		}

		resourceUsages = append(resourceUsages, resourceUsage)
	}

	sortResourceUsages(resourceUsages, existingOrder)

	usageFile.ResourceUsages = resourceUsages

	return syncResult
}

func mergeResourceUsages(dest *ResourceUsage, src *ResourceUsage, opts MergeResourceUsagesOpts) {
	if dest == nil || src == nil {
		return
	}

	destItemMap := make(map[string]*schema.UsageItem, len(dest.Items))
	for _, item := range dest.Items {
		destItemMap[item.Key] = item
	}

	for _, srcItem := range src.Items {
		destItem, ok := destItemMap[srcItem.Key]
		if !ok {
			destItem = &schema.UsageItem{Key: srcItem.Key, ValueType: srcItem.ValueType}
			dest.Items = append(dest.Items, destItem)
		}

		if opts.OverrideValueType {
			destItem.ValueType = srcItem.ValueType
		}

		if srcItem.Description != "" {
			destItem.Description = srcItem.Description
		}

		if srcItem.ValueType == schema.SubResourceUsage {
			if srcItem.DefaultValue != nil {
				srcDefaultValue := srcItem.DefaultValue.(*ResourceUsage)
				if destItem.DefaultValue == nil {
					destItem.DefaultValue = &ResourceUsage{
						Name: srcDefaultValue.Name,
					}
				}
				mergeResourceUsages(destItem.DefaultValue.(*ResourceUsage), srcDefaultValue, opts)
			}

			if srcItem.Value != nil {
				srcValue := srcItem.Value.(*ResourceUsage)
				if destItem.Value == nil {
					destItem.Value = destItem.DefaultValue
				}
				if destItem.Value == nil {
					destItem.Value = &ResourceUsage{
						Name: srcValue.Name,
					}
				}
				mergeResourceUsages(destItem.Value.(*ResourceUsage), srcValue, opts)
			}
		} else {
			if srcItem.DefaultValue != nil {
				destItem.DefaultValue = srcItem.DefaultValue
			}

			if srcItem.Value != nil {
				destItem.Value = srcItem.Value
			}
		}
	}
}

func mergeResourceUsageWithUsageData(resourceUsage *ResourceUsage, usageData *schema.UsageData) {
	if usageData == nil {
		return
	}

	for _, item := range resourceUsage.Items {
		var val interface{}

		switch item.ValueType {
		case schema.Int64:
			if v := usageData.GetInt(item.Key); v != nil {
				val = *v
			}
		case schema.Float64:
			if v := usageData.GetFloat(item.Key); v != nil {
				val = *v
			}
		case schema.String:
			if v := usageData.GetString(item.Key); v != nil {
				val = *v
			}
		case schema.StringArray:
			if v := usageData.GetStringArray(item.Key); v != nil {
				val = *v
			}
		case schema.SubResourceUsage:
			subUsageMap := usageData.Get(item.Key).Map()
			subExisting := schema.NewUsageData(item.Key, subUsageMap)

			var subResourceUsage *ResourceUsage
			// If the item has a value, use it as the base
			if item.Value != nil {
				subResourceUsage = item.Value.(*ResourceUsage)
			}

			// If the resource usage is nil, but the usage data we want to merge has data
			// for any of its sub-items, we want to add the sub-items in first before we merge
			if item.Value == nil && item.DefaultValue != nil {
				subResourceUsage = &ResourceUsage{
					Name: item.Key,
				}

				hasSubItems := false
				for _, subItem := range item.DefaultValue.(*ResourceUsage).Items {
					if subExisting.Get(subItem.Key).Type != gjson.Null {
						hasSubItems = true
						subResourceUsage.Items = append(subResourceUsage.Items, subItem)
					}
				}

				if !hasSubItems {
					subResourceUsage = nil
				}
			}

			if subResourceUsage != nil {
				mergeResourceUsageWithUsageData(subResourceUsage, subExisting)
			}

			if subResourceUsage != nil {
				val = subResourceUsage
			}
		}

		if val != nil {
			item.Value = val
		}
	}
}

// sortResourcesExistingFirst sorts the resources by the existing order first, and then the rest by name
func sortResourceUsages(resourceUsages []*ResourceUsage, existingOrder []string) {
	sort.Slice(resourceUsages, func(i, j int) bool {
		iExistingIndex := indexOf(resourceUsages[i].Name, existingOrder)
		jExistingIndex := indexOf(resourceUsages[j].Name, existingOrder)

		// If both resources are in the existing resource order, sort by the existing resource order
		if iExistingIndex != -1 && jExistingIndex != -1 {
			return iExistingIndex < jExistingIndex
		}

		// If one resource is in the existing resource order, sort it first
		if iExistingIndex == -1 && jExistingIndex != -1 {
			return false
		}
		if jExistingIndex == -1 && iExistingIndex != -1 {
			return true
		}

		// If neither resource is in the existing resource order, sort resources that have a value first
		iHasUsage := resourceUsageHasValue(resourceUsages[i])
		jHasUsafe := resourceUsageHasValue(resourceUsages[j])
		if iHasUsage && !jHasUsafe {
			return true
		}
		if jHasUsafe && !iHasUsage {
			return false
		}

		// Otherwise sort by name
		return resourceUsages[i].Name < resourceUsages[j].Name
	})
}

func resourceUsageHasValue(resourceUsage *ResourceUsage) bool {
	for _, item := range resourceUsage.Items {
		if item.Value != nil {
			return true
		}
	}
	return false
}

func indexOf(s string, arr []string) int {
	for k, v := range arr {
		if s == v {
			return k
		}
	}
	return -1
}
