package artifact

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

const (
	folderJournalDirectory  = ".agentsview-journal"
	folderJournalHeadName   = "head.json"
	folderJournalTempPrefix = ".agentsview-journal.tmp-"
	folderJournalMaxBytes   = int64(4 << 10)
)

type folderJournalHead struct {
	Sequence int64 `json:"sequence"`
}

type folderJournalEvent struct {
	Kind     Kind   `json:"kind"`
	Name     string `json:"name"`
	Origin   string `json:"origin"`
	Sequence int64  `json:"sequence"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
}

type folderJournalRejection struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func folderJournalEventName(sequence int64) string {
	return fmt.Sprintf("event-%020d.json", sequence)
}

func (t *folderTransport) appendFolderJournalLocked(
	ctx context.Context,
	entry Entry,
) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	journal, err := t.ensureFolderSubrootLocked(
		t.root,
		folderJournalDirectory,
		"journal",
	)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, journal.Close()) }()
	head, err := readFolderJournalHead(journal)
	if err != nil {
		return err
	}
	wire, err := ToWireRef(entry.Ref)
	if err != nil {
		return err
	}
	event := folderJournalEvent{
		Kind:     entry.Ref.Kind,
		Name:     wire.Name,
		Origin:   entry.Ref.Origin,
		Sequence: head.Sequence + 1,
		SHA256:   entry.Identity.SHA256,
		Size:     entry.Identity.Size,
	}
	existing, err := t.installFolderJournalEventLocked(journal, event)
	if err != nil {
		return err
	}
	if existing != nil && *existing != event {
		if err := t.writeFolderJournalHeadLocked(journal, folderJournalHead{
			Sequence: existing.Sequence,
		}); err != nil {
			return err
		}
		event.Sequence++
		occupied, err := t.installFolderJournalEventLocked(journal, event)
		if err != nil {
			return err
		}
		if occupied != nil && *occupied != event {
			return fmt.Errorf(
				"%w: artifact journal recovery sequence is occupied",
				ErrArtifactConflict,
			)
		}
	}
	return t.writeFolderJournalHeadLocked(journal, folderJournalHead{
		Sequence: event.Sequence,
	})
}

func (t *folderTransport) installFolderJournalEventLocked(
	journal *os.Root,
	event folderJournalEvent,
) (*folderJournalEvent, error) {
	body, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	name := folderJournalEventName(event.Sequence)
	err = t.writeFolderFileExclusiveLocked(journal, name, body)
	if err == nil {
		return nil, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	existing, readErr := readFolderJournalEvent(journal, event.Sequence)
	if readErr == nil {
		return &existing, nil
	}
	if !errors.Is(readErr, ErrArtifactInvalid) {
		return nil, errors.Join(err, readErr)
	}
	if quarantineErr := t.quarantineFolderEntryLocked(journal, name); quarantineErr != nil {
		return nil, errors.Join(err, readErr, quarantineErr)
	}
	if retryErr := t.writeFolderFileExclusiveLocked(journal, name, body); retryErr != nil {
		return nil, errors.Join(err, readErr, retryErr)
	}
	return nil, nil
}

func readFolderJournalHead(root *os.Root) (folderJournalHead, error) {
	file, _, err := openFolderRegularFile(root, folderJournalHeadName)
	if errors.Is(err, fs.ErrNotExist) {
		return folderJournalHead{}, nil
	}
	if err != nil {
		return folderJournalHead{}, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, folderJournalMaxBytes+1))
	if err != nil {
		return folderJournalHead{}, err
	}
	if int64(len(body)) > folderJournalMaxBytes {
		return folderJournalHead{}, fmt.Errorf("%w: artifact journal head is too large", ErrArtifactInvalid)
	}
	var head folderJournalHead
	if err := decodeCanonicalFolderJSON(body, &head); err != nil {
		return folderJournalHead{}, fmt.Errorf("%w: invalid artifact journal head: %v", ErrArtifactInvalid, err)
	}
	if head.Sequence < 0 {
		return folderJournalHead{}, fmt.Errorf("%w: invalid artifact journal sequence", ErrArtifactInvalid)
	}
	return head, nil
}

func readFolderJournalEvent(
	root *os.Root,
	sequence int64,
) (folderJournalEvent, error) {
	file, _, err := openFolderRegularFile(root, folderJournalEventName(sequence))
	if err != nil {
		return folderJournalEvent{}, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, folderJournalMaxBytes+1))
	if err != nil {
		return folderJournalEvent{}, err
	}
	if int64(len(body)) > folderJournalMaxBytes {
		return folderJournalEvent{}, fmt.Errorf("%w: artifact journal event is too large", ErrArtifactInvalid)
	}
	var event folderJournalEvent
	if err := decodeCanonicalFolderJSON(body, &event); err != nil {
		return folderJournalEvent{}, fmt.Errorf("%w: invalid artifact journal event: %v", ErrArtifactInvalid, err)
	}
	if event.Sequence != sequence {
		return folderJournalEvent{}, fmt.Errorf("%w: artifact journal sequence mismatch", ErrArtifactInvalid)
	}
	ref, err := FromWireRef(event.Origin, event.Kind, event.Name)
	if err != nil {
		return folderJournalEvent{}, err
	}
	identity, err := NewIdentity(event.SHA256, event.Size)
	if err != nil {
		return folderJournalEvent{}, err
	}
	if err := validateRefIdentity(ref, identity); err != nil {
		return folderJournalEvent{}, err
	}
	return event, nil
}

func decodeCanonicalFolderJSON(body []byte, destination any) error {
	if err := json.Unmarshal(
		body, destination, json.RejectUnknownMembers(true),
	); err != nil {
		return err
	}
	canonical, err := canonicalJSON(destination)
	if err != nil {
		return err
	}
	if !bytes.Equal(body, canonical) {
		return errors.New("noncanonical JSON")
	}
	return nil
}

func (t *folderTransport) writeFolderJournalHeadLocked(
	root *os.Root,
	head folderJournalHead,
) (retErr error) {
	body, err := json.Marshal(head)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tempName, file, err := createFolderTemp(root, folderJournalTempPrefix)
	if err != nil {
		return err
	}
	tempExists := true
	defer func() {
		if tempExists {
			retErr = errors.Join(retErr, removeFolderFile(root, tempName))
		}
	}()
	if _, err := file.Write(body); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := root.Rename(tempName, folderJournalHeadName); err != nil {
		return err
	}
	tempExists = false
	return t.syncFolderDirectoryLocked(root)
}

func (t *folderTransport) writeFolderFileExclusiveLocked(
	root *os.Root,
	name string,
	body []byte,
) (retErr error) {
	tempName, file, err := createFolderTemp(root, folderJournalTempPrefix)
	if err != nil {
		return err
	}
	tempExists := true
	defer func() {
		if tempExists {
			retErr = errors.Join(retErr, removeFolderFile(root, tempName))
		}
	}()
	if _, err := file.Write(body); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}

	linkErr := root.Link(tempName, name)
	switch {
	case linkErr == nil:
		if err := root.Remove(tempName); err != nil {
			return err
		}
		tempExists = false
	case errors.Is(linkErr, fs.ErrExist):
		return linkErr
	default:
		_, statErr := root.Lstat(name)
		switch {
		case statErr == nil:
			return fmt.Errorf("%w: %s", fs.ErrExist, name)
		case !errors.Is(statErr, fs.ErrNotExist):
			return errors.Join(linkErr, statErr)
		}
		if renameErr := root.Rename(tempName, name); renameErr != nil {
			return errors.Join(linkErr, renameErr)
		}
		tempExists = false
	}
	return t.syncFolderDirectoryLocked(root)
}

func folderJournalRejectionName(wireName string) string {
	return wireName + ".rejected"
}

func (t *folderTransport) writeFolderJournalRejectionLocked(
	root *os.Root,
	wireName string,
	identity Identity,
) error {
	rejection := folderJournalRejection(identity)
	body, err := json.Marshal(rejection)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	err = t.writeFolderFileExclusiveLocked(
		root,
		folderJournalRejectionName(wireName),
		body,
	)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return err
	}
	validationErr := validateFolderJournalRejection(root, wireName, identity)
	if validationErr == nil {
		return nil
	}
	if !errors.Is(validationErr, ErrArtifactInvalid) {
		return validationErr
	}
	name := folderJournalRejectionName(wireName)
	if quarantineErr := t.quarantineFolderEntryLocked(root, name); quarantineErr != nil {
		return errors.Join(err, validationErr, quarantineErr)
	}
	if retryErr := t.writeFolderFileExclusiveLocked(root, name, body); retryErr != nil {
		return errors.Join(err, validationErr, retryErr)
	}
	return nil
}

func validateFolderJournalRejection(
	root *os.Root,
	wireName string,
	identity Identity,
) error {
	file, _, err := openFolderRegularFile(
		root,
		folderJournalRejectionName(wireName),
	)
	if err != nil {
		return err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, folderJournalMaxBytes+1))
	if err != nil {
		return err
	}
	var rejection folderJournalRejection
	if err := decodeCanonicalFolderJSON(body, &rejection); err != nil {
		return fmt.Errorf(
			"%w: invalid artifact journal rejection: %v",
			ErrArtifactInvalid,
			err,
		)
	}
	if rejection.SHA256 != identity.SHA256 || rejection.Size != identity.Size {
		return fmt.Errorf("%w: artifact rejection identity mismatch", ErrArtifactConflict)
	}
	return nil
}

func folderJournalRejectionExists(
	root *os.Root,
	wireName string,
	identity Identity,
) (bool, error) {
	err := validateFolderJournalRejection(root, wireName, identity)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (t *folderTransport) clearFolderJournalRejectionLocked(
	root *os.Root,
	wireName string,
) error {
	if err := root.Remove(folderJournalRejectionName(wireName)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return t.syncFolderDirectoryLocked(root)
}
