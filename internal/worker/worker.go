package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	cp "github.com/otiai10/copy"
	kmmv1beta1 "github.com/rh-ecosystem-edge/kernel-module-management/api/v1beta1"
	"github.com/rh-ecosystem-edge/kernel-module-management/internal/utils"
)

//go:generate mockgen -source=worker.go -package=worker -destination=mock_worker.go

type Worker interface {
	LoadKmod(ctx context.Context, cfg *kmmv1beta1.ModuleConfig, firmwareMountPath string) error
	SetFirmwareClassPath(value string) error
	UnloadKmod(ctx context.Context, cfg *kmmv1beta1.ModuleConfig, firmwareMountPath string) error
}

type worker struct {
	im     ImageMounter
	logger logr.Logger
	mr     ModprobeRunner
}

func NewWorker(im ImageMounter, mr ModprobeRunner, logger logr.Logger) Worker {
	return &worker{
		im:     im,
		logger: logger,
		mr:     mr,
	}
}

func (w *worker) LoadKmod(ctx context.Context, cfg *kmmv1beta1.ModuleConfig, firmwareMountPath string) error {
	imageName := cfg.ContainerImage

	fsDir, err := w.im.MountImage(ctx, imageName, cfg)
	if err != nil {
		return fmt.Errorf("failed to mount image %s: %v", imageName, err)
	}

	if inTree := cfg.InTreeModuleToRemove; inTree != "" {
		w.logger.Info("Unloading in-tree module", "name", inTree)

		if err = w.mr.Run(ctx, "-rv", inTree); err != nil {
			return fmt.Errorf("could not remove in-tree module %s: %v", inTree, err)
		}
	}

	// prepare firmware
	if cfg.Modprobe.FirmwarePath != "" {
		imageFirmwarePath := filepath.Join(fsDir, cfg.Modprobe.FirmwarePath)
		w.logger.Info("preparing firmware for loading", "image directory", imageFirmwarePath, "host mount directory", firmwareMountPath)
		options := cp.Options{
			OnError: func(src, dest string, err error) error {
				if err != nil {
					return fmt.Errorf("internal copy error: failed to copy from %s to %s: %v", src, dest, err)
				}
				return nil
			},
		}
		if err = cp.Copy(imageFirmwarePath, firmwareMountPath, options); err != nil {
			return fmt.Errorf("failed to copy firmware from path %s to path %s: %v", imageFirmwarePath, firmwareMountPath, err)
		}
	}

	moduleName := cfg.Modprobe.ModuleName

	var args []string

	if cfg.Modprobe.RawArgs != nil {
		args = cfg.Modprobe.RawArgs.Load
	} else {
		args = []string{"-vd", filepath.Join(fsDir, cfg.Modprobe.DirName)}

		if cfg.Modprobe.Args != nil {
			args = append(args, cfg.Modprobe.Args.Load...)
		}

		args = append(args, moduleName)
		args = append(args, cfg.Modprobe.Parameters...)
	}

	return w.mr.Run(ctx, args...)
}

var firmwareClassPathLocation = FirmwareClassPathLocation

func (w *worker) SetFirmwareClassPath(value string) error {
	orig, err := os.ReadFile(firmwareClassPathLocation)
	if err != nil {
		return fmt.Errorf("could not read %s: %v", firmwareClassPathLocation, err)
	}

	origStr := string(orig)

	w.logger.V(1).Info("Read current firmware_class.path", "value", origStr)

	if string(orig) != value {
		w.logger.V(1).Info("Writing new firmware_class.path", "value", value)

		// 0666 set by os.Create; reuse that
		if err = os.WriteFile(firmwareClassPathLocation, []byte(value), 0666); err != nil {
			return fmt.Errorf("could not write %q into %s: %v", value, firmwareClassPathLocation, err)
		}
	}

	return nil
}

func (w *worker) UnloadKmod(ctx context.Context, cfg *kmmv1beta1.ModuleConfig, firmwareMountPath string) error {
	imageName := cfg.ContainerImage

	fsDir, err := w.im.MountImage(ctx, imageName, cfg)
	if err != nil {
		return fmt.Errorf("failed to mount image %s: %v", imageName, err)
	}

	moduleName := cfg.Modprobe.ModuleName

	var args []string

	if cfg.Modprobe.RawArgs != nil {
		args = cfg.Modprobe.RawArgs.Unload
	} else {
		args = []string{"-rvd", filepath.Join(fsDir, cfg.Modprobe.DirName)}

		if cfg.Modprobe.Args != nil {
			args = append(args, cfg.Modprobe.Args.Unload...)
		}

		args = append(args, moduleName)
	}

	w.logger.Info("Unloading module", "name", moduleName)

	if err = w.mr.Run(ctx, args...); err != nil {
		return fmt.Errorf("could not unload module %s: %v", moduleName, err)
	}

	//remove firmware files only (no directories)
	if cfg.Modprobe.FirmwarePath != "" {
		imageFirmwarePath := filepath.Join(fsDir, cfg.Modprobe.FirmwarePath)
		err = filepath.Walk(imageFirmwarePath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				relPath, err := filepath.Rel(imageFirmwarePath, path)
				if err != nil {
					w.logger.Info(utils.WarnString("failed to get relative path"), "imageFirmwarePath", imageFirmwarePath, "path", path, "error", err)
					return nil
				}
				fileToRemove := filepath.Join(firmwareMountPath, relPath)
				w.logger.Info("Removing firmware file", "file", fileToRemove)
				err = os.Remove(fileToRemove)
				if err != nil {
					w.logger.Info(utils.WarnString("failed to delete file"), "file", fileToRemove, "error", err)
				}
			}
			return nil
		})
		if err != nil {
			w.logger.Info(utils.WarnString("failed to remove all firmware blobs"), "error", err)
		}
	}

	return nil
}
