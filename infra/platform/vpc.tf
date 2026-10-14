# Network: 3 AZs, three subnet tiers.
#
#   private /19 x3  nodes and pods (VPC CNI gives each pod a VPC IP, so these are big)
#   public  /24 x3  internet-facing load balancers and the NAT gateway
#   intra   /24 x3  EKS control-plane network interfaces; no route to the internet

module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "6.7.3"

  name = var.name
  cidr = var.vpc_cidr
  azs  = var.availability_zone_ids # the module accepts zone IDs as well as names

  private_subnets = [for i in range(3) : cidrsubnet(var.vpc_cidr, 3, i)]       # 10.40.0.0/19, .32.0/19, .64.0/19
  public_subnets  = [for i in range(3) : cidrsubnet(var.vpc_cidr, 8, 96 + i)]  # 10.40.96.0/24 ...
  intra_subnets   = [for i in range(3) : cidrsubnet(var.vpc_cidr, 8, 104 + i)] # 10.40.104.0/24 ...

  enable_nat_gateway     = true
  single_nat_gateway     = var.single_nat_gateway
  one_nat_gateway_per_az = !var.single_nat_gateway

  enable_dns_hostnames = true
  enable_dns_support   = true

  # Tags the AWS Load Balancer Controller and Karpenter use to find subnets.
  public_subnet_tags = {
    "kubernetes.io/role/elb" = "1"
  }
  private_subnet_tags = {
    "kubernetes.io/role/internal-elb" = "1"
    "karpenter.sh/discovery"          = var.name
  }

  # Flow logs: who talked to whom, for incident investigation. Aggregated every minute, kept briefly.
  enable_flow_log                                 = true
  create_flow_log_cloudwatch_log_group            = true
  create_flow_log_cloudwatch_iam_role             = true
  flow_log_max_aggregation_interval               = 60
  flow_log_cloudwatch_log_group_retention_in_days = var.log_retention_days
  vpc_flow_log_iam_role_name                      = "${var.name}-vpc-flow-log"
  vpc_flow_log_iam_policy_name                    = "${var.name}-vpc-flow-log"
}

# Free gateway endpoint: image layers from ECR and other S3 traffic skip the NAT gateway,
# which charges per GB processed.
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = module.vpc.vpc_id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = concat(module.vpc.private_route_table_ids, module.vpc.intra_route_table_ids)

  tags = {
    Name = "${var.name}-s3"
  }
}
