package main

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/controlclient"
	"github.com/aphronio/dorf/internal/hostclientconfig"
)

const deploymentHostClientName = "deployment-host"

type hostEnrollmentCreator interface {
	CreateEnrollment(context.Context) (controlauth.Enrollment, error)
}

type hostControlClient interface {
	Me(context.Context) (controlapi.Identity, error)
	RedeemEnrollment(context.Context, string, string) (controlapi.Identity, error)
}

func ensureHostControlClient(ctx context.Context, store controlauth.Store, stateDir string) (controlapi.Identity, error) {
	auth := controlauth.Service{Store: store}
	return reconcileHostControlClient(ctx, auth, hostclientconfig.Path(stateDir), controlauth.GenerateCredential, func(credential string) (hostControlClient, error) {
		return controlclient.NewLoopback(credential)
	})
}

func reconcileHostControlClient(
	ctx context.Context,
	enrollments hostEnrollmentCreator,
	path string,
	generateCredential func() (string, error),
	newClient func(string) (hostControlClient, error),
) (controlapi.Identity, error) {
	stored, found, err := hostclientconfig.Load(path)
	if err != nil {
		return controlapi.Identity{}, err
	}
	if found {
		client, err := newClient(stored.Credential)
		if err != nil {
			return controlapi.Identity{}, err
		}
		identity, authErr := client.Me(ctx)
		if authErr == nil {
			if stored.EnrollmentCode != "" {
				stored.EnrollmentCode = ""
				if err := hostclientconfig.Save(path, stored); err != nil {
					return controlapi.Identity{}, err
				}
			}
			return identity, nil
		}
		if !problemCode(authErr, "unauthenticated") {
			return controlapi.Identity{}, fmt.Errorf("authenticate deployment-host Client: %w", authErr)
		}
		if stored.EnrollmentCode != "" {
			if identity, redeemErr := client.RedeemEnrollment(ctx, stored.EnrollmentCode, deploymentHostClientName); redeemErr == nil {
				stored.EnrollmentCode = ""
				if err := hostclientconfig.Save(path, stored); err != nil {
					return controlapi.Identity{}, err
				}
				return identity, nil
			} else if !problemCode(redeemErr, "enrollment_unavailable") && !problemCode(redeemErr, "client_conflict") {
				return controlapi.Identity{}, fmt.Errorf("resume deployment-host Client enrollment: %w", redeemErr)
			}
		}
	}

	credential, err := generateCredential()
	if err != nil {
		return controlapi.Identity{}, fmt.Errorf("generate deployment-host Client credential: %w", err)
	}
	enrollment, err := enrollments.CreateEnrollment(ctx)
	if err != nil {
		return controlapi.Identity{}, fmt.Errorf("create deployment-host Client enrollment: %w", err)
	}
	stored = hostclientconfig.Config{Credential: credential, EnrollmentCode: enrollment.Token}
	if err := hostclientconfig.Save(path, stored); err != nil {
		return controlapi.Identity{}, err
	}
	client, err := newClient(credential)
	if err != nil {
		return controlapi.Identity{}, err
	}
	identity, err := client.RedeemEnrollment(ctx, enrollment.Token, deploymentHostClientName)
	if err != nil {
		return controlapi.Identity{}, fmt.Errorf("enroll deployment-host Client: %w", err)
	}
	stored.EnrollmentCode = ""
	if err := hostclientconfig.Save(path, stored); err != nil {
		return controlapi.Identity{}, err
	}
	return identity, nil
}
