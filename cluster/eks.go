package cluster

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/pricing"
	"github.com/banzaicloud/pipeline/helm"
	"github.com/banzaicloud/pipeline/model"
	pkgCluster "github.com/banzaicloud/pipeline/pkg/cluster"
	"github.com/banzaicloud/pipeline/pkg/cluster/amazon"
	pkgEks "github.com/banzaicloud/pipeline/pkg/cluster/eks"
	"github.com/banzaicloud/pipeline/pkg/cluster/eks/action"
	pkgCommon "github.com/banzaicloud/pipeline/pkg/common"
	pkgErrors "github.com/banzaicloud/pipeline/pkg/errors"
	"github.com/banzaicloud/pipeline/secret"
	"github.com/banzaicloud/pipeline/secret/verify"
	"github.com/banzaicloud/pipeline/utils"
	"k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api/v1"
	"strings"
)

const mapRolesTemplate = `- rolearn: %s
  username: system:node:{{EC2PrivateDNSName}}
  groups:
  - system:bootstrappers
  - system:nodes
`

const mapUsersTemplate = `- userarn: %s
  username: %s
  groups:
  - system:masters
`

//CreateEKSClusterFromRequest creates ClusterModel struct from the request
func CreateEKSClusterFromRequest(request *pkgCluster.CreateClusterRequest, orgId uint, userId uint) (*EKSCluster, error) {
	log.Debug("Create ClusterModel struct from the request")
	var cluster EKSCluster

	modelNodePools := createNodePoolsFromRequest(request.Properties.CreateClusterEks.NodePools, userId)

	cluster.modelCluster = &model.ClusterModel{
		Name:           request.Name,
		Location:       request.Location,
		Cloud:          request.Cloud,
		OrganizationId: orgId,
		SecretId:       request.SecretId,

		Eks: model.AmazonEksClusterModel{
			Version:   request.Properties.CreateClusterEks.Version,
			NodePools: modelNodePools,
		},
	}
	return &cluster, nil
}

//EKSCluster struct for EKS cluster
type EKSCluster struct {
	eksCluster               *eks.Cluster //Don't use this directly
	modelCluster             *model.ClusterModel
	APIEndpoint              string
	CertificateAuthorityData []byte
	awsAccessKeyID           string
	awsSecretAccessKey       string
	CommonClusterBase
}

// GetOrganizationId gets org where the cluster belongs
func (e *EKSCluster) GetOrganizationId() uint {
	return e.modelCluster.OrganizationId
}

// GetSecretId retrieves the secret id
func (e *EKSCluster) GetSecretId() string {
	return e.modelCluster.SecretId
}

// GetSshSecretId retrieves the secret id
func (e *EKSCluster) GetSshSecretId() string {
	return e.modelCluster.SshSecretId
}

// SaveSshSecretId saves the ssh secret id to database
func (e *EKSCluster) SaveSshSecretId(sshSecretId string) error {
	return e.modelCluster.UpdateSshSecret(sshSecretId)
}

//GetAPIEndpoint returns the Kubernetes Api endpoint
func (e *EKSCluster) GetAPIEndpoint() (string, error) {
	return e.APIEndpoint, nil
}

//CreateEKSClusterFromModel creates ClusterModel struct from the model
func CreateEKSClusterFromModel(clusterModel *model.ClusterModel) (*EKSCluster, error) {
	log.Debug("Create ClusterModel struct from the request")
	eksCluster := EKSCluster{
		modelCluster: clusterModel,
	}
	return &eksCluster, nil
}

func (e *EKSCluster) createAWSCredentialsFromSecret() (*credentials.Credentials, error) {
	clusterSecret, err := e.GetSecretWithValidation()
	if err != nil {
		return nil, err
	}
	return verify.CreateAWSCredentials(clusterSecret.Values), nil
}

// CreateCluster creates an EKS cluster with cloudformation templates.
func (e *EKSCluster) CreateCluster() error {
	log.Info("Start creating EKS cluster")

	awsCred, err := e.createAWSCredentialsFromSecret()
	if err != nil {
		return err
	}

	session, err := session.NewSession(&aws.Config{
		Region:      aws.String(e.modelCluster.Location),
		Credentials: awsCred,
	})
	if err != nil {
		return err
	}

	// role that controls access to resources for creating an EKS cluster

	roleName := e.generateIAMRoleNameForCluster()
	eksStackName := e.generateStackNameForCluster()
	sshKeyName := e.generateSSHKeyNameForCluster()

	creationContext := action.NewEksClusterCreationContext(
		session,
		e.modelCluster.Name,
		sshKeyName,
	)

	sshSecret, err := e.GetSshSecretWithValidation()
	if err != nil {
		return err
	}

	actions := []utils.Action{
		action.NewEnsureIAMRoleAction(creationContext, roleName),
		action.NewCreateVPCAction(creationContext, eksStackName),
		action.NewUploadSSHKeyAction(creationContext, sshSecret),
		action.NewGenerateVPCConfigRequestAction(creationContext, eksStackName),
		action.NewCreateEksClusterAction(creationContext, e.modelCluster.Eks.Version),
		action.NewLoadEksSettingsAction(creationContext),
	}

	for _, nodePool := range e.modelCluster.Eks.NodePools {
		nodePoolStackName := e.generateNodePoolStackName(nodePool.Name)
		createNodePoolAction := action.NewCreateUpdateNodePoolStackAction(true, creationContext, nodePoolStackName, nodePool)
		actions = append(actions, createNodePoolAction)
	}

	_, err = utils.NewActionExecutor(log).ExecuteActions(actions, nil, true)
	if err != nil {
		log.Errorln("EKS cluster create error:", err.Error())
		return err
	}

	e.APIEndpoint = *creationContext.APIEndpoint
	e.CertificateAuthorityData, err = base64.StdEncoding.DecodeString(aws.StringValue(creationContext.CertificateAuthorityData))

	if err != nil {
		log.Errorf("Decoding base64 format EKS K8S certificate authority data failed: %s", err.Error())
		return err
	}

	// TODO make this an action
	iamSvc := iam.New(session)

	user, err := iamSvc.CreateUser(&iam.CreateUserInput{
		UserName: aws.String(e.modelCluster.Name),
	})
	if err != nil {
		return err
	}

	accessKey, err := iamSvc.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: user.User.UserName})

	// Create the aws-auth ConfigMap for letting other nodes join, and users access the API
	// See: https://docs.aws.amazon.com/eks/latest/userguide/add-user-role.html

	bootstrapCredentials, _ := awsCred.Get()
	e.awsAccessKeyID = bootstrapCredentials.AccessKeyID
	e.awsSecretAccessKey = bootstrapCredentials.SecretAccessKey

	defer func() {
		e.awsAccessKeyID = aws.StringValue(accessKey.AccessKey.AccessKeyId)
		e.awsSecretAccessKey = aws.StringValue(accessKey.AccessKey.SecretAccessKey)
		// AWS needs some time to distribute the access key to every service
		time.Sleep(15 * time.Second)
	}()

	kubeConfig, err := e.DownloadK8sConfig()
	if err != nil {
		return err
	}

	restKubeConfig, err := helm.GetK8sClientConfig(kubeConfig)
	if err != nil {
		return err
	}

	kubeClient, err := kubernetes.NewForConfig(restKubeConfig)
	if err != nil {
		return err
	}

	// create default storage class
	err = createDefaultStorageClass(kubeClient)
	if err != nil {
		return err
	}

	awsAuthConfigMap, err := generateAwsAuthConfigMap(kubeClient, user.User, creationContext.NodeInstanceRoles)
	if err != nil {
		return err
	}
	_, err = kubeClient.CoreV1().ConfigMaps("kube-system").Create(&awsAuthConfigMap)
	if err != nil {
		return err
	}

	e.modelCluster.Eks.AccessKeyID = aws.StringValue(accessKey.AccessKey.AccessKeyId)
	err = e.modelCluster.Save()
	if err != nil {
		return err
	}

	log.Infoln("EKS cluster created:", e.modelCluster.Name)

	return nil
}

func generateAwsAuthConfigMap(kubeClient *kubernetes.Clientset, user *iam.User, nodeInstanceRoles []string) (v1.ConfigMap, error) {
	mapRoles := ""
	for _, roleArn := range nodeInstanceRoles {
		log.Debugf("add nodepool role arn: %v", roleArn)
		mapRoles += fmt.Sprintf(mapRolesTemplate, roleArn)
	}
	mapUsers := fmt.Sprintf(mapUsersTemplate, aws.StringValue(user.Arn), aws.StringValue(user.UserName))
	return v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "aws-auth"},
		Data: map[string]string{
			"mapRoles": mapRoles,
			"mapUsers": mapUsers,
		},
	}, nil
}

func (e *EKSCluster) generateSSHKeyNameForCluster() string {
	sshKeyName := "ssh-key-for-cluster-" + e.modelCluster.Name
	return sshKeyName
}

func (e *EKSCluster) generateNodePoolStackName(nodePoolName string) string {
	return e.modelCluster.Name + "-pipeline-eks-nodepool-" + nodePoolName
}

func (e *EKSCluster) generateStackNameForCluster() string {
	eksStackName := e.modelCluster.Name + "-pipeline-eks"
	return eksStackName
}

func (e *EKSCluster) generateIAMRoleNameForCluster() string {
	roleName := (e.modelCluster.Name) + "-pipeline-eks"
	return roleName
}

// Persist saves the cluster model
func (e *EKSCluster) Persist(status, statusMessage string) error {
	log.Infof("Model before save: %v", e.modelCluster)
	return e.modelCluster.UpdateStatus(status, statusMessage)
}

// GetName returns the name of the cluster
func (e *EKSCluster) GetName() string {
	return e.modelCluster.Name
}

// GetType returns the cloud type of the cluster
func (e *EKSCluster) GetType() string {
	return e.modelCluster.Cloud
}

// DeleteCluster deletes cluster from google
func (e *EKSCluster) DeleteCluster() error {
	log.Info("Start delete EKS cluster")

	awsCred, err := e.createAWSCredentialsFromSecret()
	if err != nil {
		return err
	}

	session, err := session.NewSession(&aws.Config{
		Region:      aws.String(e.modelCluster.Location),
		Credentials: awsCred,
	})
	if err != nil {
		return err
	}

	deleteContext := action.NewEksClusterDeleteContext(
		session,
		e.modelCluster.Name,
	)
	actions := []utils.Action{
		action.NewWaitResourceDeletionAction(deleteContext),
		action.NewDeleteClusterAction(deleteContext),
		action.NewDeleteSSHKeyAction(deleteContext, e.generateSSHKeyNameForCluster()),
		action.NewDeleteStackAction(deleteContext, e.generateStackNameForCluster()),
		action.NewDeleteIAMRoleAction(deleteContext, e.generateIAMRoleNameForCluster()),
		action.NewDeleteUserAction(deleteContext, e.modelCluster.Name, e.modelCluster.Eks.AccessKeyID),
	}

	for _, nodePool := range e.modelCluster.Eks.NodePools {
		nodePoolStackName := e.generateNodePoolStackName(nodePool.Name)
		createStackAction := action.NewDeleteStackAction(deleteContext, nodePoolStackName)
		actions = append(actions, createStackAction)
	}

	_, err = utils.NewActionExecutor(log).ExecuteActions(actions, nil, false)
	if err != nil {
		log.Errorln("EKS cluster delete error:", err.Error())
		return err
	}

	return nil
}

func createNodePoolsFromUpdateRequest(requestedNodePools map[string]*amazon.NodePool,
	currentNodePools []*model.AmazonNodePoolsModel, userId uint) ([]*model.AmazonNodePoolsModel, error) {

	currentNodePoolMap := make(map[string]*model.AmazonNodePoolsModel, len(currentNodePools))
	for _, nodePool := range currentNodePools {
		currentNodePoolMap[nodePool.Name] = nodePool
	}

	updatedNodePools := make([]*model.AmazonNodePoolsModel, 0, len(requestedNodePools))

	for nodePoolName, nodePool := range requestedNodePools {
		if currentNodePoolMap[nodePoolName] != nil {
			// update existing node pool
			updatedNodePools = append(updatedNodePools, &model.AmazonNodePoolsModel{
				ID:             currentNodePoolMap[nodePoolName].ID,
				CreatedBy:      currentNodePoolMap[nodePoolName].CreatedBy,
				CreatedAt:      currentNodePoolMap[nodePoolName].CreatedAt,
				ClusterModelId: currentNodePoolMap[nodePoolName].ClusterModelId,
				Name:           nodePoolName,
				Autoscaling:    nodePool.Autoscaling,
				NodeMinCount:   nodePool.MinCount,
				NodeMaxCount:   nodePool.MaxCount,
				Count:          nodePool.Count,
				Delete:         false,
			})

		} else {
			// new node pool

			// ---- [ Node instanceType check ] ---- //
			if len(nodePool.InstanceType) == 0 {
				log.Errorf("instanceType is missing for nodePool %v", nodePoolName)
				return nil, pkgErrors.ErrorInstancetypeFieldIsEmpty
			}

			// ---- [ Node image check ] ---- //
			if len(nodePool.Image) == 0 {
				log.Errorf("image is missing for nodePool %v", nodePoolName)
				return nil, pkgErrors.ErrorAmazonImageFieldIsEmpty
			}

			// ---- [ Node spot price ] ---- //
			if len(nodePool.SpotPrice) == 0 {
				nodePool.SpotPrice = amazon.DefaultSpotPrice
			}

			updatedNodePools = append(updatedNodePools, &model.AmazonNodePoolsModel{
				CreatedBy:        userId,
				Name:             nodePoolName,
				NodeInstanceType: nodePool.InstanceType,
				NodeImage:        nodePool.Image,
				NodeSpotPrice:    nodePool.SpotPrice,
				Autoscaling:      nodePool.Autoscaling,
				NodeMinCount:     nodePool.MinCount,
				NodeMaxCount:     nodePool.MaxCount,
				Count:            nodePool.Count,
				Delete:           false,
			})
		}
	}

	for _, nodePool := range currentNodePools {
		if requestedNodePools[nodePool.Name] == nil {
			updatedNodePools = append(updatedNodePools, &model.AmazonNodePoolsModel{
				ID:             nodePool.ID,
				CreatedBy:      nodePool.CreatedBy,
				CreatedAt:      nodePool.CreatedAt,
				ClusterModelId: nodePool.ClusterModelId,
				Name:           nodePool.Name,
				Delete:         true,
			})
		}
	}
	return updatedNodePools, nil
}

// UpdateCluster updates EKS cluster in cloud
func (e *EKSCluster) UpdateCluster(updateRequest *pkgCluster.UpdateClusterRequest, updatedBy uint) error {
	log.Info("Start updating EKS cluster")

	awsCred, err := e.createAWSCredentialsFromSecret()
	if err != nil {
		return err
	}

	session, err := session.NewSession(&aws.Config{
		Region:      aws.String(e.modelCluster.Location),
		Credentials: awsCred,
	})
	if err != nil {
		return err
	}

	actions := make([]utils.Action, 0, len(updateRequest.Eks.NodePools))

	clusterStackName := e.generateStackNameForCluster()
	describeStacksInput := &cloudformation.DescribeStacksInput{StackName: aws.String(clusterStackName)}
	cloudformationSrv := cloudformation.New(session)
	autoscalingSrv := autoscaling.New(session)
	describeStacksOutput, err := cloudformationSrv.DescribeStacks(describeStacksInput)
	if err != nil {
		return nil
	}

	var vpcId, subnetIds, securityGroupId string
	for _, output := range describeStacksOutput.Stacks[0].Outputs {
		switch *output.OutputKey {
		case "SecurityGroups":
			securityGroupId = *output.OutputValue
		case "VpcId":
			vpcId = *output.OutputValue
		case "SubnetIds":
			subnetIds = *output.OutputValue
		}
	}

	if len(securityGroupId) == 0 {
		return errors.New("securityGroupId output not found on stack: " + clusterStackName)
	}
	if len(vpcId) == 0 {
		return errors.New("vpcId output not found on stack: " + clusterStackName)
	}
	if len(subnetIds) == 0 {
		return errors.New("subnetIds output not found on stack: " + clusterStackName)
	}

	modelNodePools, err := createNodePoolsFromUpdateRequest(updateRequest.Eks.NodePools, e.modelCluster.Eks.NodePools, updatedBy)
	if err != nil {
		return err
	}

	createUpdateContext := action.NewEksClusterUpdateContext(
		session,
		e.modelCluster.Name,
		&securityGroupId,
		aws.StringSlice(strings.Split(subnetIds, ",")),
		e.generateSSHKeyNameForCluster(),
		&vpcId)

	deleteContext := action.NewEksClusterDeleteContext(
		session,
		e.modelCluster.Name,
	)

	for _, nodePool := range modelNodePools {

		stackName := e.generateNodePoolStackName(nodePool.Name)
		describeStacksInput := &cloudformation.DescribeStacksInput{StackName: aws.String(stackName)}
		describeStacksOutput, err := cloudformationSrv.DescribeStacks(describeStacksInput)
		if err == nil {
			// delete nodePool
			if nodePool.Delete {
				log.Infof("nodePool %v exists will be deleted", nodePool.Name)
				deleteStackAction := action.NewDeleteStackAction(deleteContext, e.generateNodePoolStackName(nodePool.Name))
				actions = append(actions, deleteStackAction)
				continue
			}
			// update nodePool
			log.Infof("nodePool %v already exists will be updated", nodePool.Name)
			// load params which are not updatable from nodeGroup Stack
			for _, param := range describeStacksOutput.Stacks[0].Parameters {
				switch *param.ParameterKey {
				case "NodeImageId":
					nodePool.NodeImage = *param.ParameterValue
				case "NodeInstanceType":
					nodePool.NodeInstanceType = *param.ParameterValue
				case "NodeSpotPrice":
					nodePool.NodeSpotPrice = *param.ParameterValue
				}
			}
			// get current Desired count from ASG linked to nodeGroup stack if Autoscaling is enabled, as we don't to override
			// in this case only min/max counts
			group, err := getAutoScalingGroup(cloudformationSrv, autoscalingSrv, stackName)
			if err != nil {
				log.Errorf("unable to find Asg for stack: %v", stackName)
				return err
			}

			// override nodePool.Count with current DesiredCapacity in case of autoscale, as we don't want allow direct
			// setting of DesiredCapacity via API, however we have to limit it to be between new min, max values.
			if nodePool.Autoscaling {
				if group.DesiredCapacity != nil {
					nodePool.Count = int(*group.DesiredCapacity)
				}
				if nodePool.Count < nodePool.NodeMinCount {
					nodePool.Count = nodePool.NodeMinCount
				}
				if nodePool.Count > nodePool.NodeMaxCount {
					nodePool.Count = nodePool.NodeMaxCount
				}
				log.Infof("DesiredCapacity for %v will be: %v", *group.AutoScalingGroupARN, nodePool.Count)
			}

			updateStackAction := action.NewCreateUpdateNodePoolStackAction(false, createUpdateContext, e.generateNodePoolStackName(nodePool.Name), nodePool)
			actions = append(actions, updateStackAction)
		} else {
			if nodePool.Delete {
				log.Warnf("nodePool %v to be deleted doesn't exists: %v", nodePool.Name, err)
				continue
			}
			// create nodePool
			log.Infof("nodePool %v doesn't exists will be created", nodePool.Name)
			createNodePoolAction := action.NewCreateUpdateNodePoolStackAction(true, createUpdateContext, e.generateNodePoolStackName(nodePool.Name), nodePool)
			actions = append(actions, createNodePoolAction)
		}
	}

	_, err = utils.NewActionExecutor(log).ExecuteActions(actions, nil, false)
	if err != nil {
		log.Errorln("EKS cluster update error:", err.Error())
		return err
	}

	iamSvc := iam.New(session)
	user, err := iamSvc.GetUser(&iam.GetUserInput{
		UserName: aws.String(e.modelCluster.Name),
	})
	if err != nil {
		return err
	}

	config, err := e.GetK8sConfig()
	if err != nil {
		return err
	}
	kubeClient, err := helm.GetK8sConnection(config)
	if err != nil {
		return err
	}

	awsAuthConfigMap, err := generateAwsAuthConfigMap(kubeClient, user.User, createUpdateContext.NodeInstanceRoles)
	if err != nil {
		return err
	}
	_, err = kubeClient.CoreV1().ConfigMaps("kube-system").Update(&awsAuthConfigMap)
	if err != nil {
		return err
	}

	e.modelCluster.Eks.NodePools = modelNodePools

	return nil
}

func getAutoScalingGroup(cloudformationSrv *cloudformation.CloudFormation, autoscalingSrv *autoscaling.AutoScaling, stackName string) (*autoscaling.Group, error) {
	logResourceId := "NodeGroup"
	describeStackResourceInput := &cloudformation.DescribeStackResourceInput{
		LogicalResourceId: &logResourceId,
		StackName:         aws.String(stackName)}
	describeStacksOutput, err := cloudformationSrv.DescribeStackResource(describeStackResourceInput)
	if err != nil {
		return nil, err
	}

	describeAutoScalingGroupsInput := autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []*string{
			describeStacksOutput.StackResourceDetail.PhysicalResourceId,
		},
	}
	describeAutoScalingGroupsOutput, err := autoscalingSrv.DescribeAutoScalingGroups(&describeAutoScalingGroupsInput)
	if err != nil {
		return nil, err
	}

	return describeAutoScalingGroupsOutput.AutoScalingGroups[0], nil
}

// GenerateK8sConfig generates kube config for this EKS cluster which authenticates through the aws-iam-authenticator,
// you have to install with: go get github.com/kubernetes-sigs/aws-iam-authenticator/cmd/aws-iam-authenticator
func (e *EKSCluster) GenerateK8sConfig() *clientcmdapi.Config {
	return &clientcmdapi.Config{
		APIVersion: "v1",
		Clusters: []clientcmdapi.NamedCluster{
			{
				Name: e.modelCluster.Name,
				Cluster: clientcmdapi.Cluster{
					Server: e.APIEndpoint,
					CertificateAuthorityData: e.CertificateAuthorityData,
				},
			},
		},
		Contexts: []clientcmdapi.NamedContext{
			{
				Name: e.modelCluster.Name,
				Context: clientcmdapi.Context{
					AuthInfo: "eks",
					Cluster:  e.modelCluster.Name,
				},
			},
		},
		AuthInfos: []clientcmdapi.NamedAuthInfo{
			{
				Name: "eks",
				AuthInfo: clientcmdapi.AuthInfo{
					Exec: &clientcmdapi.ExecConfig{
						APIVersion: "client.authentication.k8s.io/v1alpha1",
						Command:    "aws-iam-authenticator",
						Args:       []string{"token", "-i", e.modelCluster.Name},
						Env: []clientcmdapi.ExecEnvVar{
							{Name: "AWS_ACCESS_KEY_ID", Value: e.awsAccessKeyID},
							{Name: "AWS_SECRET_ACCESS_KEY", Value: e.awsSecretAccessKey},
						},
					},
				},
			},
		},
		Kind:           "Config",
		CurrentContext: e.modelCluster.Name,
	}
}

// DownloadK8sConfig generates and marshalls the kube config for this cluster.
func (e *EKSCluster) DownloadK8sConfig() ([]byte, error) {
	config := e.GenerateK8sConfig()
	return json.Marshal(config)
}

// GetStatus describes the status of this EKS cluster.
func (e *EKSCluster) GetStatus() (*pkgCluster.GetClusterStatusResponse, error) {

	nodePools := make(map[string]*pkgCluster.NodePoolStatus)
	for _, np := range e.modelCluster.Eks.NodePools {
		if np != nil {
			nodePools[np.Name] = &pkgCluster.NodePoolStatus{
				Autoscaling:  np.Autoscaling,
				Count:        np.Count,
				InstanceType: np.NodeInstanceType,
				SpotPrice:    np.NodeSpotPrice,
				MinCount:     np.NodeMinCount,
				MaxCount:     np.NodeMaxCount,
				Image:        np.NodeImage,
			}
		}
	}

	return &pkgCluster.GetClusterStatusResponse{
		Status:        e.modelCluster.Status,
		StatusMessage: e.modelCluster.StatusMessage,
		Name:          e.modelCluster.Name,
		Location:      e.modelCluster.Location,
		Cloud:         e.modelCluster.Cloud,
		ResourceID:    e.modelCluster.ID,
		NodePools:     nodePools,
	}, nil
}

// GetID returns the DB ID of this cluster
func (e *EKSCluster) GetID() uint {
	return e.modelCluster.ID
}

// GetModel returns the DB model of this cluster
func (e *EKSCluster) GetModel() *model.ClusterModel {
	return e.modelCluster
}

// CheckEqualityToUpdate validates the update request
func (e *EKSCluster) CheckEqualityToUpdate(r *pkgCluster.UpdateClusterRequest) error {
	return CheckEqualityToUpdate(r, e.modelCluster.Eks.NodePools)
}

// AddDefaultsToUpdate adds defaults to update request
func (e *EKSCluster) AddDefaultsToUpdate(r *pkgCluster.UpdateClusterRequest) {
	defaultImage := pkgEks.DefaultImages[e.modelCluster.Location]

	// add default node image(s) if needed
	if r != nil && r.Eks != nil && r.Eks.NodePools != nil {
		for _, np := range r.Eks.NodePools {
			if len(np.Image) == 0 {
				np.Image = defaultImage
			}
		}
	}
}

// DeleteFromDatabase deletes model from the database
func (e *EKSCluster) DeleteFromDatabase() error {
	err := e.modelCluster.Delete()
	if err != nil {
		return err
	}
	e.modelCluster = nil
	return nil
}

// ListNodeNames returns node names to label them
func (e *EKSCluster) ListNodeNames() (nodeNames pkgCommon.NodeNames, err error) {
	// nodes are labeled in create request
	return
}

// UpdateStatus updates cluster status in database
func (e *EKSCluster) UpdateStatus(status string, statusMessage string) error {
	return e.modelCluster.UpdateStatus(status, statusMessage)
}

// GetClusterDetails gets cluster details from cloud
func (e *EKSCluster) GetClusterDetails() (*pkgCluster.DetailsResponse, error) {
	log.Infoln("Start getting cluster details")

	awsCred, err := e.createAWSCredentialsFromSecret()
	if err != nil {
		return nil, err
	}

	session, err := session.NewSession(&aws.Config{
		Region:      aws.String(e.modelCluster.Location),
		Credentials: awsCred,
	})
	if err != nil {
		return nil, err
	}

	eksSvc := eks.New(session)
	describeCluster := &eks.DescribeClusterInput{Name: aws.String(e.GetName())}
	clusterDesc, err := eksSvc.DescribeCluster(describeCluster)
	if err != nil {
		return nil, err
	}

	nodePools := make(map[string]*pkgCluster.NodeDetails)
	for _, np := range e.modelCluster.Eks.NodePools {
		if np != nil {
			nodePools[np.Name] = &pkgCluster.NodeDetails{
				CreatorBaseFields: *NewCreatorBaseFields(np.CreatedAt, np.CreatedBy),
				Version:           aws.StringValue(clusterDesc.Cluster.Version),
			}
		}
	}

	if aws.StringValue(clusterDesc.Cluster.Status) == eks.ClusterStatusActive {
		return &pkgCluster.DetailsResponse{
			CreatorBaseFields: *NewCreatorBaseFields(e.modelCluster.CreatedAt, e.modelCluster.CreatedBy),
			Name:              e.modelCluster.Name,
			Id:                e.modelCluster.ID,
			Location:          e.modelCluster.Location,
			MasterVersion:     aws.StringValue(clusterDesc.Cluster.Version),
			NodePools:         nodePools,
			Endpoint:          e.APIEndpoint,
		}, nil
	}

	return nil, pkgErrors.ErrorClusterNotReady
}

// ValidateCreationFields validates all fields
func (e *EKSCluster) ValidateCreationFields(r *pkgCluster.CreateClusterRequest) error {
	regions, err := ListEksRegions(e.GetOrganizationId(), e.GetSecretId())
	if err != nil {
		log.Errorf("Listing regions where EKS service is available failed: %s", err.Error())
		return err
	}

	regionFound := false
	for _, region := range regions {
		if region == r.Location {
			regionFound = true
			break
		}
	}

	if !regionFound {
		return pkgErrors.ErrorNotValidLocation
	}

	imagesInRegion, err := ListEksImages(r.Location)
	if err != nil {
		log.Errorf("Listing AMIs that that support EKS failed: %s", err.Error())
		return err
	}

	for name, nodePool := range r.Properties.CreateClusterEks.NodePools {
		images, ok := imagesInRegion[r.Location]
		if !ok {
			log.Errorf("Image %q provided for node pool %q is not valid", name, nodePool.Image)
			return pkgErrors.ErrorNotValidNodeImage
		}

		for _, image := range images {
			if image != nodePool.Image {
				log.Errorf("Image %q provided for node pool %q is not valid", name, nodePool.Image)
				return pkgErrors.ErrorNotValidNodeImage
			}
		}

	}

	return nil
}

// GetSecretWithValidation returns secret from vault
func (e *EKSCluster) GetSecretWithValidation() (*secret.SecretItemResponse, error) {
	return e.CommonClusterBase.getSecret(e)
}

// GetSshSecretWithValidation returns ssh secret from vault
func (e *EKSCluster) GetSshSecretWithValidation() (*secret.SecretItemResponse, error) {
	return e.CommonClusterBase.getSshSecret(e)
}

// SaveConfigSecretId saves the config secret id in database
func (e *EKSCluster) SaveConfigSecretId(configSecretId string) error {
	return e.modelCluster.UpdateConfigSecret(configSecretId)
}

// GetConfigSecretId return config secret id
func (e *EKSCluster) GetConfigSecretId() string {
	return e.modelCluster.ConfigSecretId
}

// GetK8sConfig returns the Kubernetes config
func (e *EKSCluster) GetK8sConfig() ([]byte, error) {
	return e.CommonClusterBase.getConfig(e)
}

// RequiresSshPublicKey returns true as a public ssh key is needed for bootstrapping
// the cluster
func (e *EKSCluster) RequiresSshPublicKey() bool {
	return true
}

// ReloadFromDatabase load cluster from DB
func (e *EKSCluster) ReloadFromDatabase() error {
	return e.modelCluster.ReloadFromDatabase()
}

// ListEksRegions returns the regions in which AmazonEKS service is enabled
func ListEksRegions(orgId uint, secretId string) ([]string, error) {
	// AWS API https://docs.aws.amazon.com/sdk-for-go/api/aws/endpoints/ doesn't recognizes AmazonEKS service yet
	// thus we can not use it to query what locations the service is enabled in.

	// We'll use the pricing API to determine what locations the service is enabled in.

	// TODO revisit this later when https://docs.aws.amazon.com/sdk-for-go/api/aws/endpoints/ starts supporting AmazonEKS

	secret, err := secret.Store.Get(orgId, secretId)
	if err != nil {
		return nil, err
	}

	credentials := verify.CreateAWSCredentials(secret.Values)
	session, err := session.NewSession(&aws.Config{
		Region:      aws.String(pkgEks.UsEast1), // pricing API available in us-east-1
		Credentials: credentials,
	})
	if err != nil {
		return nil, err
	}

	svc := pricing.New(session)

	getAttrValuesInput := &pricing.GetAttributeValuesInput{
		AttributeName: aws.String(pkgCluster.KeyWordLocation),
		ServiceCode:   aws.String("AmazonEKS"),
	}
	attributeValues, err := svc.GetAttributeValues(getAttrValuesInput)
	if err != nil {
		return nil, err
	}

	var eksLocations []string
	for _, attrValue := range attributeValues.AttributeValues {
		eksLocations = append(eksLocations, aws.StringValue(attrValue.Value))
	}

	resolver := endpoints.DefaultResolver()
	partitions := resolver.(endpoints.EnumPartitions).Partitions()

	var eksRegionIds []string
	for _, p := range partitions {
		for _, r := range p.Regions() {
			for _, eksLocation := range eksLocations {
				if r.Description() == eksLocation {
					eksRegionIds = append(eksRegionIds, r.ID())
				}
			}
		}

	}

	return eksRegionIds, nil
}

// ListEksImages returns AMIs for EKS
func ListEksImages(region string) (map[string][]string, error) {
	// currently there are only two AMIs for EKS.
	// TODO: revise this once there is AWS API for retrieving EKS AMIs dynamically at runtime
	ami, ok := pkgEks.DefaultImages[region]
	if ok {
		return map[string][]string{
			region: {ami},
		}, nil
	}

	return map[string][]string{
		region: {},
	}, nil
}

// createDefaultStorageClass creates a default storage class as Amazon EKS clusters are not created with
// any storage classes
func createDefaultStorageClass(kubernetesClient *kubernetes.Clientset) error {
	defaultStorageClass := storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "default",
			Annotations: map[string]string{
				"storageclass.kubernetes.io/is-default-class": "true",
			},
		},
		Provisioner: "kubernetes.io/aws-ebs",
		Parameters: map[string]string{
			"type": "gp2",
		},
	}

	_, err := kubernetesClient.StorageV1().StorageClasses().Create(&defaultStorageClass)

	return err
}
