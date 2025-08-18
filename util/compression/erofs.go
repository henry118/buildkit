package compression

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/labels"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const MediaTypeImageLayerEROFS = "application/vnd.oci.image.layer.erofs"

func (c erofsType) Compress(ctx context.Context, comp Config) (compressorFunc Compressor, finalize Finalizer) {
	dgst := digest.SHA256.Digester()
	return func(dest io.Writer, _ string) (io.WriteCloser, error) {
			tmpImg, err := os.CreateTemp("", "erofs-img-*.erofs")
			if err != nil {
				return nil, fmt.Errorf("failed to create temp erofs image: %w", err)
			}
			tmpImgPath := tmpImg.Name()
			tmpImg.Close()

			cmd := exec.CommandContext(ctx, "mkfs.erofs", "--tar=f", "--aufs", "--quiet", "-zlz4hc", "-Enoinline_data", tmpImgPath)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				os.Remove(tmpImgPath)
				return nil, fmt.Errorf("failed to get stdin pipe: %w", err)
			}
			if err := cmd.Start(); err != nil {
				os.Remove(tmpImgPath)
				return nil, fmt.Errorf("failed to start mkfs.erofs: %w", err)
			}
			return &erofsWriteCloser{
				cmd:     cmd,
				stdin:   stdin,
				imgPath: tmpImgPath,
				dest:    dest,
				dgst:    dgst,
			}, nil
		}, func(ctx context.Context, cs content.Store) (map[string]string, error) {
			info, err := cs.Info(ctx, dgst.Digest())
			if err != nil {
				return nil, errors.Wrap(err, "failed to get info from content store")
			}
			if info.Labels == nil {
				info.Labels = make(map[string]string)
			}
			info.Labels[labels.LabelUncompressed] = dgst.Digest().String()
			if _, err := cs.Update(ctx, info, "labels."+labels.LabelUncompressed); err != nil {
				return nil, err
			}
			a := make(map[string]string)
			a[labels.LabelUncompressed] = dgst.Digest().String()
			return a, nil
		}
}

func (c erofsType) Decompress(ctx context.Context, cs content.Store, desc ocispecs.Descriptor) (io.ReadCloser, error) {
	// Get EROFS image from content store
	r, err := cs.ReaderAt(ctx, desc)
	if err != nil {
		return nil, err
	}
	tmpImg, err := os.CreateTemp("", "erofs-img-*.erofs")
	if err != nil {
		return nil, err
	}
	defer tmpImg.Close()
	_, err = io.Copy(tmpImg, content.NewReader(r))
	if err != nil {
		os.Remove(tmpImg.Name())
		return nil, err
	}

	// Use mount.WithTempMount to mount the EROFS image
	mnt := mount.Mount{
		Type:    "erofs",
		Source:  tmpImg.Name(),
		Options: []string{},
	}
	pr, pw := io.Pipe()
	go func() {
		_ = mount.WithTempMount(ctx, []mount.Mount{mnt}, func(root string) error {
			// Tar the mounted directory and write to pipe
			tarCmd := exec.CommandContext(ctx, "tar", "-C", root, "-cf", "-", ".")
			tarCmd.Stdout = pw
			tarCmd.Stderr = nil
			_ = tarCmd.Run()
			pw.Close()
			return nil
		})
		os.Remove(tmpImg.Name())
	}()
	return pr, nil
}

func (c erofsType) NeedsConversion(ctx context.Context, cs content.Store, desc ocispecs.Descriptor) (bool, error) {
	if !images.IsLayerType(desc.MediaType) {
		return false, nil
	}
	return desc.MediaType != MediaTypeImageLayerEROFS, nil
}

func (c erofsType) NeedsComputeDiffBySelf(comp Config) bool {
	return true
}

func (c erofsType) OnlySupportOCITypes() bool {
	return true
}

func (c erofsType) MediaType() string {
	return MediaTypeImageLayerEROFS
}

func (c erofsType) String() string {
	return "erofs"
}

type erofsWriteCloser struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	imgPath string
	dest    io.Writer
	dgst    digest.Digester
}

func (w *erofsWriteCloser) Write(p []byte) (int, error) {
	return w.stdin.Write(p)
}

func (w *erofsWriteCloser) Close() error {
	w.stdin.Close()
	if err := w.cmd.Wait(); err != nil {
		return fmt.Errorf("mkfs.erofs command failed: %w", err)
	}
	imgFile, err := os.Open(w.imgPath)
	if err != nil {
		return fmt.Errorf("failed to open erofs image %q: %w", w.imgPath, err)
	}
	defer imgFile.Close()
	if _, err = io.Copy(w.dest, io.TeeReader(imgFile, w.dgst.Hash())); err != nil {
		return fmt.Errorf("failed to copy erofs image %q to destination: %w", w.imgPath, err)
	}
	return os.Remove(w.imgPath)
}
