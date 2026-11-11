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
