package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/canonical/lxd-imagebuilder/shared"
	"github.com/canonical/lxd-imagebuilder/simplestream-maintainer/stream"
)

type pruneOptions struct {
	global *globalOptions

	Dangling      bool
	RetainBuilds  int
	RetainDays    int
	StreamVersion string
	ImageDirs     []string
}

func (o *pruneOptions) NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "prune <path> [flags]",
		Short:   "Prune product versions",
		Long:    "Prune product versions except for latest retaining only the specific number of latest ones.",
		GroupID: "main",
		RunE:    o.Run,
	}

	cmd.PersistentFlags().BoolVar(&o.Dangling, "dangling", false, "Remove dangling product versions (not referenced from any product catalog)")
	cmd.PersistentFlags().IntVar(&o.RetainBuilds, "retain-builds", 10, "Maximum number of product versions to retain")
	cmd.PersistentFlags().IntVar(&o.RetainDays, "retain-days", 0, "Maximum number of days to retain any product version")
	cmd.PersistentFlags().StringVar(&o.StreamVersion, "stream-version", "v1", "Stream version")
	cmd.PersistentFlags().StringSliceVarP(&o.ImageDirs, "image-dir", "d", []string{"images"}, "Image directory (relative to path argument)")

	return cmd
}

func (o *pruneOptions) Run(_ *cobra.Command, args []string) error {
	if len(args) < 1 || args[0] == "" {
		return fmt.Errorf("Argument %q is required and cannot be empty", "path")
	}

	for _, dir := range o.ImageDirs {
		if o.Dangling {
			err := pruneDanglingProductVersions(args[0], o.StreamVersion, dir)
			if err != nil {
				return err
			}
		}

		err := pruneStreamProductVersions(args[0], o.StreamVersion, dir, o.RetainBuilds, o.RetainDays)
		if err != nil {
			return err
		}
	}

	return pruneEmptyDirs(args[0], true)
}

// pruneStreamProductVersions reads the product catalog and removes all product
// versions except for the number of latests versions defined by retain integer.
func pruneStreamProductVersions(rootDir string, streamVersion string, streamName string, retainBuilds int, retainDays int) error {
	if retainBuilds < 1 {
		return fmt.Errorf("At least 1 product version build must be retained")
	}

	// Read product catalog.
	catalogPath := filepath.Join(rootDir, "streams", streamVersion, fmt.Sprintf("%s.json", streamName))
	catalog, err := shared.ReadJSONFile(catalogPath, &stream.ProductCatalog{})
	if err != nil {
		return err
	}

	// Find versions that need to be discarded.
	var discardVersions []string

	for id, p := range catalog.Products {
		productPath := filepath.Join(rootDir, streamName, p.RelPath())

		versions := shared.MapKeys(p.Versions)
		slices.Sort(versions)
		slices.Reverse(versions)

		// Extract versions that need to be discarded.
		for i, v := range versions {
			versionPath := filepath.Join(productPath, v)

			// Remove version outside the retainBuilds.
			if i >= retainBuilds {
				delete(catalog.Products[id].Versions, v)
				discardVersions = append(discardVersions, versionPath)
				continue
			}

			// Remove versions older then retainDays.
			if retainDays > 0 {
				info, err := os.Stat(versionPath)
				if err != nil {
					return err
				}

				maxAge := time.Duration(retainDays) * 24 * time.Hour
				if time.Since(info.ModTime()) > maxAge {
					delete(catalog.Products[id].Versions, v)
					discardVersions = append(discardVersions, versionPath)
				}
			}
		}
	}

	// Write product catalog to a temporary file that is located next
	// to the final file to ensure atomic replace. Temporary file is
	// prefixed with a dot to hide it.
	catalogPathTemp := filepath.Join(rootDir, "streams", streamVersion, fmt.Sprintf(".%s.json.tmp", streamName))
	err = shared.WriteJSONFile(catalogPathTemp, catalog)
	if err != nil {
		return err
	}

	defer os.Remove(catalogPathTemp)

	// Replace existing stream json file.
	err = os.Rename(catalogPathTemp, catalogPath)
	if err != nil {
		return err
	}

	// Set read permissions.
	err = os.Chmod(catalogPath, 0644)
	if err != nil {
		return err
	}

	// Remove old versions.
	for _, v := range discardVersions {
		err := os.RemoveAll(v)
		if err != nil {
			slog.Error("Failed to prune old product version", "path", v, "error", err)
			continue // Do not error out.
		}

		slog.Info("Pruned old product version", "path", v, "error", err)
	}

	return nil
}

// pruneDanglingProductVersions traverses through the stream directory structure
// and prunes the product versions that are not referenced by the corresponding
// product catalog.
func pruneDanglingProductVersions(rootDir string, streamVersion string, streamName string) error {
	// Get all products including incomplete (from actual directory hierarchy).
	products, err := stream.GetProducts(rootDir, streamName, stream.WithIncompleteVersions(true))
	if err != nil {
		return err
	}

	// Get current products (from stream json file).
	catalogPath := filepath.Join(rootDir, "streams", streamVersion, fmt.Sprintf("%s.json", streamName))
	catalog, err := shared.ReadJSONFile(catalogPath, &stream.ProductCatalog{})
	if err != nil {
		return err
	}

	// If product catalog is empty, skip removal of dangling resources, because this
	// may result in wiping everything out if, for example, product catalog was build
	// inproperly or was accidentally deleted.
	if len(catalog.Products) == 0 {
		slog.Info("Skipping removal of dangling resources, because product catalog is empty")
		return nil
	}

	// removeIfOlder gets info of the file on the given path and removes it
	// if it's modification time is older then maxAge.
	removeIfOlder := func(path string, maxAge time.Duration) error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}

		if time.Since(info.ModTime()) > maxAge {
			err := os.RemoveAll(path)
			if err != nil {
				slog.Error("Failed to prune dangling resource", "path", path, "error", err)
				return nil // Do not error out.
			}

			slog.Info("Pruned dangling resource", "path", path)
		}

		return nil
	}

	for key, rp := range products {
		productPath := filepath.Join(rootDir, streamName, rp.RelPath())

		cp, ok := catalog.Products[key]
		if !ok {
			// Remove unreferenced product if older then 6 hours.
			err := removeIfOlder(productPath, 6*time.Hour)
			if err != nil {
				return err
			}
		} else {
			// Iterate over detected versions and remove unreferenced ones.
			for rpv := range rp.Versions {
				_, ok := cp.Versions[rpv]
				if ok {
					// Version is referenced, nothing to do.
					continue
				}

				// Remove unreferenced product version if older
				// then 6 hours.
				versionPath := filepath.Join(productPath, rpv)
				err := removeIfOlder(versionPath, 6*time.Hour)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// pruneEmptyDirs traverses the file structure on the given path and
// recursively removes all empty directories. Setting keepBaseDir to
// true, ensures the function does not remove the base directory if
// it is empty.
func pruneEmptyDirs(baseDir string, keepBaseDir bool) error {
	baseDir = filepath.Clean(baseDir)

	// Read directory contents.
	files, err := os.ReadDir(baseDir)
	if err != nil {
		return err
	}

	// Traverse the files and prune directories if not empty.
	if len(files) > 0 {
		for _, f := range files {
			if !f.IsDir() {
				continue
			}

			child := filepath.Join(baseDir, f.Name())
			err = pruneEmptyDirs(child, false)
			if err != nil {
				return err
			}
		}

		// Read files again, as current directory may be empty now.
		files, err = os.ReadDir(baseDir)
		if err != nil {
			return err
		}
	}

	// Remove empty directory if it is not marked as base dir.
	if !keepBaseDir && len(files) == 0 {
		err := os.Remove(baseDir)
		if err != nil {
			return err
		}

		slog.Info("Removed empty directory", "path", baseDir)
	}

	return nil
}
