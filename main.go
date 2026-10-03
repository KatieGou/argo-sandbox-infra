package main

import (
	"fmt"
	"slices"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ecr"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

const region = "eu-north-1"

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		cfg := config.New(ctx, "")
		githubOwner := cfg.Require("githubOwner")     // KatieGou
		appRepo := cfg.Require("appRepo")             // argo-sandbox-app
		githubOwnerID := cfg.Require("githubOwnerId") // 71438233
		appRepoID := cfg.Require("appRepoId")         // 1403088479

		// 1. The AWS provider: which region, and tags stamped on every resource.
		awsProvider, err := aws.NewProvider(ctx, "aws-"+region, &aws.ProviderArgs{
			Region: pulumi.String(region),
			DefaultTags: &aws.ProviderDefaultTagsArgs{
				Tags: pulumi.StringMap{
					"environment":  pulumi.String(cfg.Require("environment")),
					"service":      pulumi.String(cfg.Require("service")),
					"created-with": pulumi.String("pulumi"),
					"repo":         pulumi.String(cfg.Require("repo")),
					"managed-by":   pulumi.String("pgt"),
				},
			},
		})
		if err != nil {
			return fmt.Errorf("creating aws provider: %w", err)
		}

		// 2. ECR repository: where CI pushes images and EKS nodes pull them from.
		repo, err := ecr.NewRepository(ctx, "app", &ecr.RepositoryArgs{
			Name:               pulumi.String(appRepo),
			ImageTagMutability: pulumi.String("IMMUTABLE"), // a tag can never point at a different image
			ImageScanningConfiguration: &ecr.RepositoryImageScanningConfigurationArgs{
				ScanOnPush: pulumi.Bool(true),
			},
			ForceDelete: pulumi.Bool(true), // sandbox: let destroy remove a repo that still has images
		}, pulumi.Provider(awsProvider))
		if err != nil {
			return fmt.Errorf("creating ecr repository: %w", err)
		}

		// 3. Reuse the GitHub OIDC provider that already exists in this account (legacy setup).
		// A lookup, not a resource: Pulumi reads it but never changes or deletes it.
		githubOidc, err := iam.LookupOpenIdConnectProvider(ctx, &iam.LookupOpenIdConnectProviderArgs{
			Url: pulumi.StringRef("https://token.actions.githubusercontent.com"),
		}, pulumi.Provider(awsProvider))
		if err != nil {
			return fmt.Errorf("looking up github oidc provider: %w", err)
		}
		if !slices.Contains(githubOidc.ClientIdLists, "sts.amazonaws.com") {
			return fmt.Errorf("github oidc provider %s does not allow audience sts.amazonaws.com", githubOidc.Arn)
		}

		// 4. Trust policy: WHO may assume the CI role.
		// Only GitHub Actions runs for tags in KatieGou/argo-sandbox-app.
		// Every value here is a plain string, so the plain GetPolicyDocument is enough.
		trustPolicy, err := iam.GetPolicyDocument(ctx, &iam.GetPolicyDocumentArgs{
			Statements: []iam.GetPolicyDocumentStatement{
				{
					Actions: []string{"sts:AssumeRoleWithWebIdentity"},
					Principals: []iam.GetPolicyDocumentStatementPrincipal{
						{Type: "Federated", Identifiers: []string{githubOidc.Arn}},
					},
					Conditions: []iam.GetPolicyDocumentStatementCondition{
						{
							Test:     "StringEquals",
							Variable: "token.actions.githubusercontent.com:aud",
							Values:   []string{"sts.amazonaws.com"},
						},
						{
							Test:     "StringLike",
							Variable: "token.actions.githubusercontent.com:sub",
							Values:   []string{fmt.Sprintf("repo:%s@%s/%s@%s:ref:refs/tags/*", githubOwner, githubOwnerID, appRepo, appRepoID)},
						},
					},
				},
			},
		}, pulumi.Provider(awsProvider))
		if err != nil {
			return fmt.Errorf("building ci role trust policy: %w", err)
		}

		ciRole, err := iam.NewRole(ctx, "ci-ecr-push", &iam.RoleArgs{
			Name:             pulumi.String("argo-sandbox-ci-ecr-push"),
			AssumeRolePolicy: pulumi.String(trustPolicy.Json),
		}, pulumi.Provider(awsProvider))
		if err != nil {
			return fmt.Errorf("creating ci role: %w", err)
		}

		// 5. Permissions policy: WHAT the CI role may do. Log in to ECR, push to this one repo.
		pushPolicy := iam.GetPolicyDocumentOutput(ctx, iam.GetPolicyDocumentOutputArgs{
			Statements: iam.GetPolicyDocumentStatementArray{
				iam.GetPolicyDocumentStatementArgs{
					Sid:       pulumi.String("EcrLogin"),
					Actions:   pulumi.StringArray{pulumi.String("ecr:GetAuthorizationToken")},
					Resources: pulumi.StringArray{pulumi.String("*")}, // this action has no resource scope
				},
				iam.GetPolicyDocumentStatementArgs{
					Sid: pulumi.String("EcrPush"),
					Actions: pulumi.StringArray{
						pulumi.String("ecr:BatchCheckLayerAvailability"),
						pulumi.String("ecr:BatchGetImage"),
						pulumi.String("ecr:InitiateLayerUpload"),
						pulumi.String("ecr:UploadLayerPart"),
						pulumi.String("ecr:CompleteLayerUpload"),
						pulumi.String("ecr:PutImage"),
					},
					Resources: pulumi.StringArray{repo.Arn},
				},
			},
		}, pulumi.Provider(awsProvider))

		_, err = iam.NewRolePolicy(ctx, "ci-ecr-push", &iam.RolePolicyArgs{
			Role:   ciRole.Name,
			Policy: pushPolicy.Json(),
		}, pulumi.Provider(awsProvider))
		if err != nil {
			return fmt.Errorf("attaching push policy to ci role: %w", err)
		}

		// Outputs: values you'll paste into the GitHub Actions workflow in Stage 2.
		ctx.Export("ecrRepositoryUrl", repo.RepositoryUrl)
		ctx.Export("ciRoleArn", ciRole.Arn)
		return nil
	})
}
