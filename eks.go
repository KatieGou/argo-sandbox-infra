package main

import (
	"fmt"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/eks"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const clusterName = "argo-sandbox-meinan"

// newEks creates everything Stage 3 needs: a small public VPC, the EKS control plane and one worker node.
func newEks(ctx *pulumi.Context, provider pulumi.ProviderResource, eksVersion string) error {
	subnetIDs, err := newPublicNetwork(ctx, provider)
	if err != nil {
		return err
	}

	// 1. Cluster role: what the EKS control plane itself may do in the account (manage ENIs, etc.).
	clusterRole, clusterAttachments, err := newServiceRole(ctx, provider, "eks-cluster", "eks.amazonaws.com", []string{
		"arn:aws:iam::aws:policy/AmazonEKSClusterPolicy",
	})
	if err != nil {
		return err
	}

	// 2. The control plane. AWS runs it; we only see the API endpoint.
	cluster, err := eks.NewCluster(ctx, "argo-sandbox", &eks.ClusterArgs{
		Name:    pulumi.String(clusterName),
		Version: pulumi.String(eksVersion),
		RoleArn: clusterRole.Arn,
		VpcConfig: &eks.ClusterVpcConfigArgs{
			SubnetIds:             subnetIDs,
			EndpointPublicAccess:  pulumi.Bool(true), // your laptop's kubectl reaches the API over the internet
			EndpointPrivateAccess: pulumi.Bool(true), // nodes reach the API from inside the VPC
		},
		AccessConfig: &eks.ClusterAccessConfigArgs{
			// Who may use kubectl is controlled by EKS access entries (not the old aws-auth ConfigMap).
			AuthenticationMode: pulumi.String("API"),
			// Whoever runs `pulumi up` (your SSO role) becomes cluster admin.
			BootstrapClusterCreatorAdminPermissions: pulumi.Bool(true),
		},
		// Never slide into extended support, which costs 6x more per hour.
		UpgradePolicy: &eks.ClusterUpgradePolicyArgs{
			SupportType: pulumi.String("STANDARD"),
		},
	}, pulumi.Provider(provider), pulumi.DependsOn(clusterAttachments))
	if err != nil {
		return fmt.Errorf("creating eks cluster: %w", err)
	}

	// 3. Node role: what the EC2 worker nodes may do. PullOnly is what lets kubelet pull from your ECR repo.
	nodeRole, nodeAttachments, err := newServiceRole(ctx, provider, "eks-node", "ec2.amazonaws.com", []string{
		"arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
		"arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
		"arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryPullOnly",
	})
	if err != nil {
		return err
	}

	// 4. Managed node group: AWS launches and joins the EC2 instances for us.
	_, err = eks.NewNodeGroup(ctx, "default", &eks.NodeGroupArgs{
		ClusterName:   cluster.Name,
		NodeGroupName: pulumi.String(clusterName + "-default"),
		NodeRoleArn:   nodeRole.Arn,
		SubnetIds:     subnetIDs,
		InstanceTypes: pulumi.StringArray{pulumi.String("t3.medium")}, // x86, 2 vCPU / 4 GiB, max 17 pods
		AmiType:       pulumi.String("AL2023_x86_64_STANDARD"),
		CapacityType:  pulumi.String("ON_DEMAND"),
		DiskSize:      pulumi.Int(20),
		ScalingConfig: &eks.NodeGroupScalingConfigArgs{
			DesiredSize: pulumi.Int(1),
			MinSize:     pulumi.Int(1),
			MaxSize:     pulumi.Int(2),
		},
	}, pulumi.Provider(provider), pulumi.DependsOn(nodeAttachments))
	if err != nil {
		return fmt.Errorf("creating eks node group: %w", err)
	}

	ctx.Export("clusterName", cluster.Name)
	ctx.Export("kubeconfigCommand", pulumi.Sprintf(
		"aws eks update-kubeconfig --name %s --region %s --profile pgt-old-dev", cluster.Name, region))
	return nil
}

// newPublicNetwork creates a VPC with two public subnets (EKS needs subnets in at least two AZs).
// No private subnets and no NAT gateway: nodes get public IPs and reach ECR and GitHub directly.
// Cheap, fine for a sandbox, not how you would run production.
func newPublicNetwork(ctx *pulumi.Context, provider pulumi.ProviderResource) (pulumi.StringArray, error) {
	azs, err := aws.GetAvailabilityZones(ctx, &aws.GetAvailabilityZonesArgs{
		State: pulumi.StringRef("available"),
	}, pulumi.Provider(provider))
	if err != nil {
		return nil, fmt.Errorf("looking up availability zones: %w", err)
	}
	if len(azs.Names) < 2 {
		return nil, fmt.Errorf("need at least 2 availability zones in %s, found %d", region, len(azs.Names))
	}

	vpc, err := ec2.NewVpc(ctx, "eks", &ec2.VpcArgs{
		CidrBlock:          pulumi.String("10.0.0.0/16"),
		EnableDnsSupport:   pulumi.Bool(true),
		EnableDnsHostnames: pulumi.Bool(true), // EKS nodes need DNS hostnames to join the cluster
		Tags:               pulumi.StringMap{"Name": pulumi.String(clusterName)},
	}, pulumi.Provider(provider))
	if err != nil {
		return nil, fmt.Errorf("creating vpc: %w", err)
	}

	igw, err := ec2.NewInternetGateway(ctx, "eks", &ec2.InternetGatewayArgs{
		VpcId: vpc.ID(),
		Tags:  pulumi.StringMap{"Name": pulumi.String(clusterName)},
	}, pulumi.Provider(provider))
	if err != nil {
		return nil, fmt.Errorf("creating internet gateway: %w", err)
	}

	// Everything not inside the VPC goes out through the internet gateway.
	routeTable, err := ec2.NewRouteTable(ctx, "public", &ec2.RouteTableArgs{
		VpcId: vpc.ID(),
		Routes: ec2.RouteTableRouteArray{
			ec2.RouteTableRouteArgs{
				CidrBlock: pulumi.String("0.0.0.0/0"),
				GatewayId: igw.ID(),
			},
		},
		Tags: pulumi.StringMap{"Name": pulumi.String(clusterName + "-public")},
	}, pulumi.Provider(provider))
	if err != nil {
		return nil, fmt.Errorf("creating route table: %w", err)
	}

	var subnetIDs pulumi.StringArray
	for i, az := range azs.Names[:2] {
		name := fmt.Sprintf("public-%d", i)
		subnet, err := ec2.NewSubnet(ctx, name, &ec2.SubnetArgs{
			VpcId:               vpc.ID(),
			AvailabilityZone:    pulumi.String(az),
			CidrBlock:           pulumi.Sprintf("10.0.%d.0/24", i),
			MapPublicIpOnLaunch: pulumi.Bool(true), // nodes get a public IP, so no NAT gateway is needed
			Tags:                pulumi.StringMap{"Name": pulumi.String(clusterName + "-" + name)},
		}, pulumi.Provider(provider))
		if err != nil {
			return nil, fmt.Errorf("creating subnet %s: %w", name, err)
		}

		_, err = ec2.NewRouteTableAssociation(ctx, name, &ec2.RouteTableAssociationArgs{
			SubnetId:     subnet.ID(),
			RouteTableId: routeTable.ID(),
		}, pulumi.Provider(provider))
		if err != nil {
			return nil, fmt.Errorf("associating route table with subnet %s: %w", name, err)
		}

		subnetIDs = append(subnetIDs, subnet.ID().ToStringOutput())
	}
	return subnetIDs, nil
}

// newServiceRole creates an IAM role that an AWS service (EKS, EC2) can assume, with managed policies attached.
// It returns the attachments so callers can depend on them: the cluster/node group must not be
// created before its permissions exist, and on destroy the permissions must outlive the cluster.
func newServiceRole(ctx *pulumi.Context, provider pulumi.ProviderResource, name, service string, policyArns []string) (*iam.Role, []pulumi.Resource, error) {
	trust, err := iam.GetPolicyDocument(ctx, &iam.GetPolicyDocumentArgs{
		Statements: []iam.GetPolicyDocumentStatement{
			{
				Actions: []string{"sts:AssumeRole"},
				Principals: []iam.GetPolicyDocumentStatementPrincipal{
					{Type: "Service", Identifiers: []string{service}},
				},
			},
		},
	}, pulumi.Provider(provider))
	if err != nil {
		return nil, nil, fmt.Errorf("building %s trust policy: %w", name, err)
	}

	role, err := iam.NewRole(ctx, name, &iam.RoleArgs{
		Name:             pulumi.String(clusterName + "-" + name),
		AssumeRolePolicy: pulumi.String(trust.Json),
	}, pulumi.Provider(provider))
	if err != nil {
		return nil, nil, fmt.Errorf("creating %s role: %w", name, err)
	}

	var attachments []pulumi.Resource
	for i, arn := range policyArns {
		attachment, err := iam.NewRolePolicyAttachment(ctx, fmt.Sprintf("%s-%d", name, i), &iam.RolePolicyAttachmentArgs{
			Role:      role.Name,
			PolicyArn: pulumi.String(arn),
		}, pulumi.Provider(provider))
		if err != nil {
			return nil, nil, fmt.Errorf("attaching %s to %s role: %w", arn, name, err)
		}
		attachments = append(attachments, attachment)
	}
	return role, attachments, nil
}
