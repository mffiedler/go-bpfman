package bpfman

import (
	"encoding/json"
	"errors"
	"fmt"
)

// LoadSpec describes how to load a BPF program.
// LoadSpec is immutable after construction and can only be created via
// the NewLoadSpec or NewAttachLoadSpec constructors, which enforce that
// all required fields are present and valid.
//
// LoadSpec represents user intent (what to load), not runtime wiring
// (where to pin). The bpffs root is provided separately by the Manager
// when calling the kernel layer.
type LoadSpec struct {
	objectPath      string
	programName     string
	programType     ProgramType
	globalData      map[string][]byte
	imageURL        string
	imageDigest     string
	imagePullPolicy ImagePullPolicy
	imageUsername   string // for registry auth
	imagePassword   string // for registry auth
	attachFunc      string
	mapOwnerID      uint32
}

// RequiresAttachFunc returns true if this program type requires an attach
// function (fentry and fexit).
func (t ProgramType) RequiresAttachFunc() bool {
	return t == ProgramTypeFentry || t == ProgramTypeFexit
}

// Valid returns true if this is a known, specified program type.
func (t ProgramType) Valid() bool {
	switch t {
	case ProgramTypeXDP, ProgramTypeTC, ProgramTypeTCX,
		ProgramTypeTracepoint, ProgramTypeKprobe, ProgramTypeKretprobe,
		ProgramTypeUprobe, ProgramTypeUretprobe,
		ProgramTypeFentry, ProgramTypeFexit:
		return true
	default:
		return false
	}
}

// NewLoadSpec creates a LoadSpec for program types that do not require
// an attach function. For fentry/fexit, use NewAttachLoadSpec instead.
//
// Returns an error if:
//   - objectPath is empty
//   - programName is empty
//   - programType is invalid or unspecified
//   - programType requires an attach function (use NewAttachLoadSpec)
func NewLoadSpec(objectPath, programName string, programType ProgramType) (LoadSpec, error) {
	if objectPath == "" {
		return LoadSpec{}, errors.New("objectPath is required")
	}
	if programName == "" {
		return LoadSpec{}, errors.New("programName is required")
	}
	if !programType.Valid() {
		return LoadSpec{}, fmt.Errorf("invalid program type: %s", programType)
	}
	if programType.RequiresAttachFunc() {
		return LoadSpec{}, fmt.Errorf("%s requires NewAttachLoadSpec with attachFunc", programType)
	}
	return LoadSpec{
		objectPath:  objectPath,
		programName: programName,
		programType: programType,
	}, nil
}

// NewAttachLoadSpec creates a LoadSpec for program types that require an
// attach function (fentry/fexit).
//
// Returns an error if:
//   - objectPath is empty
//   - programName is empty
//   - programType is invalid or does not require an attach function
//   - attachFunc is empty
func NewAttachLoadSpec(objectPath, programName string, programType ProgramType, attachFunc string) (LoadSpec, error) {
	if objectPath == "" {
		return LoadSpec{}, errors.New("objectPath is required")
	}
	if programName == "" {
		return LoadSpec{}, errors.New("programName is required")
	}
	if !programType.Valid() {
		return LoadSpec{}, fmt.Errorf("invalid program type: %s", programType)
	}
	if !programType.RequiresAttachFunc() {
		return LoadSpec{}, fmt.Errorf("%s does not require attachFunc, use NewLoadSpec", programType)
	}
	if attachFunc == "" {
		return LoadSpec{}, fmt.Errorf("attachFunc is required for %s", programType)
	}
	return LoadSpec{
		objectPath:  objectPath,
		programName: programName,
		programType: programType,
		attachFunc:  attachFunc,
	}, nil
}

// NewImageLoadSpec creates a LoadSpec for loading a program from an OCI image.
// The objectPath will be resolved when the image is pulled.
//
// This is used for the unified Load interface where the caller specifies
// an image reference and the manager handles pulling.
//
// For fentry/fexit, use NewImageAttachLoadSpec instead.
//
// Returns an error if:
//   - imageURL is empty
//   - programName is empty
//   - programType is invalid or unspecified
//   - programType requires an attach function (use NewImageAttachLoadSpec)
func NewImageLoadSpec(imageURL, programName string, programType ProgramType, pullPolicy ImagePullPolicy) (LoadSpec, error) {
	if imageURL == "" {
		return LoadSpec{}, errors.New("imageURL is required")
	}
	if programName == "" {
		return LoadSpec{}, errors.New("programName is required")
	}
	if !programType.Valid() {
		return LoadSpec{}, fmt.Errorf("invalid program type: %s", programType)
	}
	if programType.RequiresAttachFunc() {
		return LoadSpec{}, fmt.Errorf("%s requires NewImageAttachLoadSpec with attachFunc", programType)
	}
	return LoadSpec{
		programName:     programName,
		programType:     programType,
		imageURL:        imageURL,
		imagePullPolicy: pullPolicy,
	}, nil
}

// NewImageAttachLoadSpec creates a LoadSpec for loading a program from an OCI
// image that requires an attach function (fentry/fexit).
//
// Returns an error if:
//   - imageURL is empty
//   - programName is empty
//   - programType is invalid or does not require an attach function
//   - attachFunc is empty
func NewImageAttachLoadSpec(imageURL, programName string, programType ProgramType, attachFunc string, pullPolicy ImagePullPolicy) (LoadSpec, error) {
	if imageURL == "" {
		return LoadSpec{}, errors.New("imageURL is required")
	}
	if programName == "" {
		return LoadSpec{}, errors.New("programName is required")
	}
	if !programType.Valid() {
		return LoadSpec{}, fmt.Errorf("invalid program type: %s", programType)
	}
	if !programType.RequiresAttachFunc() {
		return LoadSpec{}, fmt.Errorf("%s does not require attachFunc, use NewImageLoadSpec", programType)
	}
	if attachFunc == "" {
		return LoadSpec{}, fmt.Errorf("attachFunc is required for %s", programType)
	}
	return LoadSpec{
		programName:     programName,
		programType:     programType,
		attachFunc:      attachFunc,
		imageURL:        imageURL,
		imagePullPolicy: pullPolicy,
	}, nil
}

// Getters for LoadSpec fields

func (s LoadSpec) ObjectPath() string               { return s.objectPath }
func (s LoadSpec) ProgramName() string              { return s.programName }
func (s LoadSpec) ProgramType() ProgramType         { return s.programType }
func (s LoadSpec) GlobalData() map[string][]byte    { return s.globalData }
func (s LoadSpec) ImageURL() string                 { return s.imageURL }
func (s LoadSpec) ImageDigest() string              { return s.imageDigest }
func (s LoadSpec) ImagePullPolicy() ImagePullPolicy { return s.imagePullPolicy }
func (s LoadSpec) ImageUsername() string            { return s.imageUsername }
func (s LoadSpec) ImagePassword() string            { return s.imagePassword }
func (s LoadSpec) AttachFunc() string               { return s.attachFunc }
func (s LoadSpec) MapOwnerID() uint32               { return s.mapOwnerID }

// HasImageAuth returns true if this LoadSpec has registry authentication configured.
func (s LoadSpec) HasImageAuth() bool { return s.imageUsername != "" }

// HasImageSource returns true if this LoadSpec specifies an OCI image source.
// This is true both when the image URL was set as input (for pulling) and
// when it was set as provenance (after loading from an image).
func (s LoadSpec) HasImageSource() bool { return s.imageURL != "" }

// IsImageLoad returns true if this LoadSpec should load from an OCI image.
// This differs from HasImageSource in that it returns true only when the
// image URL is set AND the objectPath is not set (meaning we need to pull).
func (s LoadSpec) IsImageLoad() bool { return s.imageURL != "" && s.objectPath == "" }

// WithGlobalData returns a new LoadSpec with global data set.
func (s LoadSpec) WithGlobalData(data map[string][]byte) LoadSpec {
	s.globalData = data
	return s
}

// WithImageProvenance returns a new LoadSpec with image provenance set.
// Used when loading from an OCI image.
func (s LoadSpec) WithImageProvenance(url, digest string, policy ImagePullPolicy) LoadSpec {
	s.imageURL = url
	s.imageDigest = digest
	s.imagePullPolicy = policy
	return s
}

// WithMapOwnerID returns a new LoadSpec with map owner ID set.
func (s LoadSpec) WithMapOwnerID(id uint32) LoadSpec {
	s.mapOwnerID = id
	return s
}

// WithImageAuth returns a new LoadSpec with registry authentication set.
func (s LoadSpec) WithImageAuth(username, password string) LoadSpec {
	s.imageUsername = username
	s.imagePassword = password
	return s
}

// Builder methods for reconstructing LoadSpec from stored data.
// These bypass constructor validation since the data was validated at creation time.

// WithObjectPath returns a new LoadSpec with object path set.
// Used when reconstructing from stored data.
func (s LoadSpec) WithObjectPath(path string) LoadSpec {
	s.objectPath = path
	return s
}

// WithProgramName returns a new LoadSpec with program name set.
// Used when reconstructing from stored data.
func (s LoadSpec) WithProgramName(name string) LoadSpec {
	s.programName = name
	return s
}

// WithProgramType returns a new LoadSpec with program type set.
// Used when reconstructing from stored data.
func (s LoadSpec) WithProgramType(pt ProgramType) LoadSpec {
	s.programType = pt
	return s
}

// WithAttachFunc returns a new LoadSpec with attach function set.
// Used when reconstructing from stored data.
func (s LoadSpec) WithAttachFunc(fn string) LoadSpec {
	s.attachFunc = fn
	return s
}

// imageSourceJSON is the JSON representation of image provenance fields.
// Kept as a nested object for backwards compatibility with existing DB rows.
type imageSourceJSON struct {
	URL        string          `json:"url"`
	Digest     string          `json:"digest,omitempty"`
	PullPolicy ImagePullPolicy `json:"pull_policy,omitempty"`
}

// loadSpecJSON is the JSON representation of LoadSpec.
// This allows LoadSpec to have private fields while still being serializable.
type loadSpecJSON struct {
	ObjectPath  string            `json:"object_path"`
	ProgramName string            `json:"program_name"`
	ProgramType ProgramType       `json:"program_type"`
	GlobalData  map[string][]byte `json:"global_data,omitempty"`
	ImageSource *imageSourceJSON  `json:"image_source,omitempty"`
	AttachFunc  string            `json:"attach_func,omitempty"`
	MapOwnerID  uint32            `json:"map_owner_id,omitempty"`
}

// MarshalJSON implements json.Marshaler.
func (s LoadSpec) MarshalJSON() ([]byte, error) {
	var imgSrc *imageSourceJSON
	if s.imageURL != "" {
		imgSrc = &imageSourceJSON{
			URL:        s.imageURL,
			Digest:     s.imageDigest,
			PullPolicy: s.imagePullPolicy,
		}
	}
	return json.Marshal(loadSpecJSON{
		ObjectPath:  s.objectPath,
		ProgramName: s.programName,
		ProgramType: s.programType,
		GlobalData:  s.globalData,
		ImageSource: imgSrc,
		AttachFunc:  s.attachFunc,
		MapOwnerID:  s.mapOwnerID,
	})
}

// UnmarshalJSON implements json.Unmarshaler.
// Note: This bypasses the constructor validation to support deserializing
// stored data. The assumption is that data was validated at creation time.
func (s *LoadSpec) UnmarshalJSON(data []byte) error {
	var js loadSpecJSON
	if err := json.Unmarshal(data, &js); err != nil {
		return err
	}
	s.objectPath = js.ObjectPath
	s.programName = js.ProgramName
	s.programType = js.ProgramType
	s.globalData = js.GlobalData
	if js.ImageSource != nil {
		s.imageURL = js.ImageSource.URL
		s.imageDigest = js.ImageSource.Digest
		s.imagePullPolicy = js.ImageSource.PullPolicy
	}
	s.attachFunc = js.AttachFunc
	s.mapOwnerID = js.MapOwnerID
	return nil
}
