mock_provider "aws" {}

run "version_inventory_through_gateway_endpoint" {
  command = plan
  module { source = "../../modules/endpoints" }
  variables {
    name              = "miles-dev"
    region            = "ap-southeast-1"
    vpc_id            = "vpc-00000000000000001"
    private_subnets   = {}
    route_table_ids   = ["rtb-00000000000000001"]
    gateway_sg_id     = "sg-00000000000000001"
    jobs_sg_id        = "sg-00000000000000002"
    enable_interfaces = false
    data_bucket_name  = "miles-fixture"
    table_arns        = ["arn:aws:dynamodb:ap-southeast-1:123456789012:table/miles-fixture"]
  }
  assert {
    condition = alltrue([
      for statement in jsondecode(aws_vpc_endpoint.gateway["s3"].policy).Statement :
      contains(statement.Action, "s3:ListBucketVersions") && statement.Resource == ["arn:aws:s3:::miles-fixture", "arn:aws:s3:::miles-fixture/*"]
    ])
    error_message = "The S3 gateway endpoint must permit inventory of retained versions in the data bucket."
  }
}

run "private_edge_roles_anywhere_endpoint" {
  command = plan
  module { source = "../../modules/endpoints" }
  variables {
    name                            = "miles-dev"
    region                          = "ap-southeast-1"
    vpc_id                          = "vpc-00000000000000001"
    private_subnets                 = { a = "subnet-00000000000000001" }
    route_table_ids                 = ["rtb-00000000000000001"]
    gateway_sg_id                   = "sg-00000000000000001"
    jobs_sg_id                      = "sg-00000000000000002"
    enable_interfaces               = true
    enable_roles_anywhere           = true
    roles_anywhere_trust_anchor_arn = "arn:aws:rolesanywhere:ap-southeast-1:123456789012:trust-anchor/test"
    data_bucket_name                = "miles-fixture"
    table_arns                      = ["arn:aws:dynamodb:ap-southeast-1:123456789012:table/miles-fixture"]
  }
  assert {
    condition     = aws_vpc_endpoint.interface["rolesanywhere"].service_name == "com.amazonaws.ap-southeast-1.rolesanywhere" && aws_vpc_endpoint.interface["rolesanywhere"].private_dns_enabled
    error_message = "Private Edge needs a Roles Anywhere interface endpoint with private DNS."
  }
  assert {
    condition     = alltrue([for s in jsondecode(aws_vpc_endpoint.interface["rolesanywhere"].policy).Statement : s.Action == "rolesanywhere:CreateSession" && s.Resource == "arn:aws:rolesanywhere:ap-southeast-1:123456789012:trust-anchor/test"])
    error_message = "The private endpoint must restrict certificate sessions to the Edge trust anchor."
  }
}
