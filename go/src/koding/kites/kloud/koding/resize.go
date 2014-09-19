package koding

import (
	"fmt"
	"koding/kites/kloud/klient"
	"strconv"
	"time"

	"github.com/koding/kloud/machinestate"
	"github.com/koding/kloud/protocol"
	"github.com/koding/kloud/waitstate"
	"github.com/mitchellh/goamz/ec2"
)

func (p *Provider) Resize(opts *protocol.Machine) (resArtifact *protocol.Artifact, resErr error) {
	/*
		0. Check if size is eglible (not equal or less than the current size)
		1. Stop the instance
		2. Get VolumeId of current instance
		3. Get AvailabilityZone of current instance
		4. Create snapshot from that given VolumeId
		5. Delete snapshot after we are done with all following steps
		6. Create new volume with the desired size from the snapshot and same zone.
		7. Delete volume if something goes wrong in following steps
		8. Detach the volume of current stopped instance
		9. Reattach old volume if something goes wrong, if not delete it
		10. Attach new volume to current stopped instance
		11. Start the stopped instance
		12. Update Domain record with the new IP
		13. Check if Klient is running
		14. Return success
	*/

	defer p.Unlock(opts.MachineId)

	a, err := p.NewClient(opts)
	if err != nil {
		return nil, err
	}

	// 0. Check if size is eglible (not equal or less than the current size)
	// 2. Get VolumeId of current instance
	a.Log.Info("0. Checking if size is eglible for instance %s", a.Id())
	instance, err := a.Instance(a.Id())
	if err != nil {
		return nil, err
	}

	if len(instance.BlockDevices) == 0 {
		return nil, fmt.Errorf("fatal error: no block device available")
	}

	oldVolumeId := instance.BlockDevices[0].VolumeId
	oldVolResp, err := a.Client.Volumes([]string{oldVolumeId}, ec2.NewFilter())
	if err != nil {
		return nil, err
	}

	volSize := oldVolResp.Volumes[0].Size
	currentSize, err := strconv.Atoi(volSize)
	if err != nil {
		return nil, err
	}

	desiredSize := a.Builder.StorageSize

	if desiredSize <= currentSize {
		return nil, fmt.Errorf("resizing is not allowed. Desired size: %dGB should be larger than current size: %dGB",
			desiredSize, currentSize)
	}

	if 100 < desiredSize {
		return nil, fmt.Errorf("resizing is not allowed. Desired size: %d can't be larger than 100GB",
			desiredSize)
	}

	// 1. Stop the instance
	a.Log.Info("1. Stopping Machine")
	if opts.State != machinestate.Stopped {
		err = a.Stop()
		if err != nil {
			return nil, err
		}
	}

	p.UpdateState(opts.MachineId, machinestate.Pending)

	// 3. Get AvailabilityZone of current instance
	a.Log.Info("3. Getting Avail Zone")
	availZone := instance.AvailZone

	// 4. Create new snapshot from that given VolumeId
	a.Log.Info("4. Create snapshot from volume %s", oldVolumeId)
	snapshotDesc := fmt.Sprintf("Temporary snapshot for instance %s", instance.InstanceId)
	resp, err := a.Client.CreateSnapshot(oldVolumeId, snapshotDesc)
	if err != nil {
		return nil, err
	}

	newSnapshotId := resp.Id

	checkSnapshot := func(currentPercentage int) (machinestate.State, error) {
		resp, err := a.Client.Snapshots([]string{newSnapshotId}, ec2.NewFilter())
		if err != nil {
			return 0, err
		}

		if resp.Snapshots[0].Status != "completed" {
			return machinestate.Pending, nil
		}

		return machinestate.Stopped, nil
	}

	ws := waitstate.WaitState{StateFunc: checkSnapshot, DesiredState: machinestate.Stopped}
	if err := ws.Wait(); err != nil {
		return nil, err
	}

	// 5. Delete snapshot after we are done with all steps
	defer a.Client.DeleteSnapshots([]string{newSnapshotId})

	// 6. Create new volume with the desired size from the snapshot and same availability zone.
	a.Log.Info("5. Create new volume from snapshot %s", newSnapshotId)
	volOptions := &ec2.CreateVolume{
		AvailZone:  availZone,
		Size:       int64(desiredSize),
		SnapshotId: newSnapshotId,
		VolumeType: "gp2", // SSD
	}

	volResp, err := a.Client.CreateVolume(volOptions)
	if err != nil {
		return nil, err
	}

	newVolumeId := volResp.VolumeId

	checkVolume := func(currentPercentage int) (machinestate.State, error) {
		resp, err := a.Client.Volumes([]string{newVolumeId}, ec2.NewFilter())
		if err != nil {
			return 0, err
		}

		if resp.Volumes[0].Status != "available" {
			return machinestate.Pending, nil
		}

		return machinestate.Stopped, nil
	}

	ws = waitstate.WaitState{StateFunc: checkVolume, DesiredState: machinestate.Stopped}
	if err := ws.Wait(); err != nil {
		return nil, err
	}

	// 7. Delete volume if something goes wrong in following steps
	defer func() {
		if resErr != nil {
			a.Log.Info("An error occured, deleting new volume %s", newVolumeId)
			_, err := a.Client.DeleteVolume(newVolumeId)
			if err != nil {
				a.Log.Error(err.Error())
			}
		}
	}()

	// 8. Detach the volume of current stopped instance
	a.Log.Info("6. Detach old volume %s", oldVolumeId)
	if _, err := a.Client.DetachVolume(oldVolumeId); err != nil {
		return nil, err
	}

	checkDetaching := func(currentPercentage int) (machinestate.State, error) {
		resp, err := a.Client.Volumes([]string{oldVolumeId}, ec2.NewFilter())
		if err != nil {
			return 0, err
		}
		vol := resp.Volumes[0]

		// ready!
		if len(vol.Attachments) == 0 {
			return machinestate.Stopped, nil
		}

		// otherwise wait until it's detached
		if vol.Attachments[0].Status != "detached" {
			return machinestate.Pending, nil
		}

		return machinestate.Stopped, nil
	}

	ws = waitstate.WaitState{StateFunc: checkDetaching, DesiredState: machinestate.Stopped}
	if err := ws.Wait(); err != nil {
		return nil, err
	}

	// 9. Reattach old volume if something goes wrong, if not delete it
	defer func() {
		// if something goes wrong  detach the newly attached volume and attach
		// back the old volume  so it can be used again
		if resErr != nil {
			a.Log.Info("An error occured, re attaching old volume %s", a.Id())
			_, err := a.Client.DetachVolume(newVolumeId)
			if err != nil {
				a.Log.Error(err.Error())
			}

			_, err = a.Client.AttachVolume(oldVolumeId, a.Id(), "/dev/sda1")
			if err != nil {
				a.Log.Error(err.Error())
			}
		} else {
			// if not just delete, it's not used anymore
			a.Log.Info("Deleting old volume %s", a.Id())
			go a.Client.DeleteVolume(oldVolumeId)
		}
	}()

	// 10. Attach new volume to current stopped instance
	if _, err := a.Client.AttachVolume(newVolumeId, a.Id(), "/dev/sda1"); err != nil {
		return nil, err
	}

	checkAttaching := func(currentPercentage int) (machinestate.State, error) {
		resp, err := a.Client.Volumes([]string{newVolumeId}, ec2.NewFilter())
		if err != nil {
			return 0, err
		}

		vol := resp.Volumes[0]

		if len(vol.Attachments) == 0 {
			return machinestate.Pending, nil
		}

		if vol.Attachments[0].Status != "attached" {
			return machinestate.Pending, nil
		}

		return machinestate.Stopped, nil
	}

	ws = waitstate.WaitState{StateFunc: checkAttaching, DesiredState: machinestate.Stopped}
	if err := ws.Wait(); err != nil {
		return nil, err
	}

	// 11. Start the stopped instance
	artifact, err := a.Start()
	if err != nil {
		return nil, err
	}

	// 12. Update Domain record with the new IP
	machineData, ok := opts.CurrentData.(*Machine)
	if !ok {
		return nil, fmt.Errorf("current data is malformed: %v", opts.CurrentData)
	}

	username := opts.Builder["username"].(string)

	if err := p.UpdateDomain(artifact.IpAddress, machineData.Domain, username); err != nil {
		return nil, err
	}

	a.Log.Info("[%s] Updating user domain tag '%s' of instance '%s'",
		opts.MachineId, machineData.Domain, artifact.InstanceId)
	if err := a.AddTag(artifact.InstanceId, "koding-domain", machineData.Domain); err != nil {
		return nil, err
	}

	artifact.DomainName = machineData.Domain

	fmt.Printf("artifact %+v\n", artifact)

	// 13. Check if Klient is running
	a.Push("Checking remote machine", 90, machinestate.Starting)
	p.Log.Info("[%s] Connecting to remote Klient instance", opts.MachineId)
	klientRef, err := klient.NewWithTimeout(p.Kite, machineData.QueryString, time.Minute*1)
	if err != nil {
		p.Log.Warning("Connecting to remote Klient instance err: %s", err)
	} else {
		defer klientRef.Close()
		p.Log.Info("[%s] Sending a ping message", opts.MachineId)
		if err := klientRef.Ping(); err != nil {
			p.Log.Warning("Sending a ping message err:", err)
		}
	}

	return artifact, nil
}
