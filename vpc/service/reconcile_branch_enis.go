package service

import (
	"context"
	"database/sql"

	"github.com/Netflix/titus-executor/aws/aws-sdk-go/aws"
	"github.com/Netflix/titus-executor/aws/aws-sdk-go/service/ec2"
	"github.com/Netflix/titus-executor/logger"
	"github.com/Netflix/titus-executor/vpc"
	"github.com/Netflix/titus-executor/vpc/service/ec2wrapper"
	"github.com/pkg/errors"
	"go.opencensus.io/trace"
)

func (vpcService *vpcService) reconcileBranchENIsForRegionAccount(ctx context.Context, account regionAccount, tx *sql.Tx) (retErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctx, span := trace.StartSpan(ctx, "reconcileBranchENIsForRegionAccount")
	defer span.End()
	span.AddAttributes(trace.StringAttribute("region", account.region), trace.StringAttribute("account", account.accountID))
	session, err := vpcService.ec2.GetSessionFromAccountAndRegion(ctx, ec2wrapper.Key{
		AccountID: account.accountID,
		Region:    account.region,
	})
	if err != nil {
		logger.G(ctx).WithError(err).Error("Could not get session")
		span.SetStatus(traceStatusFromError(err))
		return err
	}

	logger.G(ctx).WithFields(map[string]interface{}{
		"region":    account.region,
		"accountID": account.accountID,
	}).Info("Beginning reconcilation")

	_, err = tx.ExecContext(ctx, "CREATE TEMPORARY TABLE IF NOT EXISTS known_branch_enis (branch_eni TEXT PRIMARY KEY, account_id text, subnet_id text, az text, vpc_id text, state text) ON COMMIT DROP ")
	if err != nil {
		span.SetStatus(traceStatusFromError(err))
		return errors.Wrap(err, "Could not create temporary table for known branch enis")
	}

	ec2client := ec2.New(session.Session)
	describeNetworkInterfacesInput := ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("description"),
				Values: aws.StringSlice([]string{vpc.BranchNetworkInterfaceDescription}),
			},
		},
		MaxResults: aws.Int64(1000),
	}

	for {
		output, err := ec2client.DescribeNetworkInterfacesWithContext(ctx, &describeNetworkInterfacesInput)
		if err != nil {
			logger.G(ctx).WithError(err).Error("Could not describe network interfaces")
			return ec2wrapper.HandleEC2Error(err, span)
		}
		for _, branchENI := range output.NetworkInterfaces {
			_, err = tx.ExecContext(ctx, "INSERT INTO known_branch_enis(branch_eni, account_id, subnet_id, az, vpc_id, state) VALUES ($1, $2, $3, $4, $5, $6)",
				aws.StringValue(branchENI.NetworkInterfaceId),
				aws.StringValue(branchENI.OwnerId),
				aws.StringValue(branchENI.SubnetId),
				aws.StringValue(branchENI.AvailabilityZone),
				aws.StringValue(branchENI.VpcId),
				aws.StringValue(branchENI.Status),
			)
			if err != nil {
				return errors.Wrap(err, "Could not update known_branch_enis")
			}
		}
		if output.NextToken == nil {
			break
		}
		describeNetworkInterfacesInput.NextToken = output.NextToken
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO branch_enis(branch_eni, account_id, subnet_id, az, vpc_id) SELECT branch_eni, account_id, subnet_id, az, vpc_id FROM known_branch_enis ON CONFLICT (branch_eni) DO NOTHING")
	if err != nil {
		return errors.Wrap(err, "Could not insert new branch ENIs")
	}

	_, err = tx.ExecContext(ctx, "CREATE TEMPORARY TABLE IF NOT EXISTS known_branch_eni_attachments (branch_eni TEXT PRIMARY KEY, trunk_eni text, idx int, association_id text) ON COMMIT DROP")
	if err != nil {
		return errors.Wrap(err, "Could not create temporary table for known branch enis")
	}

	describeTrunkInterfaceAssociationsInput := ec2.DescribeTrunkInterfaceAssociationsInput{
		MaxResults: aws.Int64(255),
	}

	for {
		output, err := ec2client.DescribeTrunkInterfaceAssociationsWithContext(ctx, &describeTrunkInterfaceAssociationsInput)
		if err != nil {
			logger.G(ctx).WithError(err).Error()
		}
		for _, assoc := range output.InterfaceAssociations {
			_, err = tx.ExecContext(ctx, "INSERT INTO known_branch_eni_attachments(branch_eni, trunk_eni, idx, association_id) VALUES ($1, $2, $3, $4)",
				aws.StringValue(assoc.BranchInterfaceId),
				aws.StringValue(assoc.TrunkInterfaceId),
				aws.Int64Value(assoc.VlanId),
				aws.StringValue(assoc.AssociationId),
			)
			if err != nil {
				return errors.Wrap(err, "Could not update known_branch_enis")
			}
		}
		if output.NextToken == nil {
			break
		}
		describeTrunkInterfaceAssociationsInput.NextToken = output.NextToken
	}
	_, err = tx.ExecContext(ctx, "UPDATE branch_eni_attachments SET state = 'unknown' WHERE branch_eni NOT IN (SELECT branch_eni FROM known_branch_enis)")
	if err != nil {
		return errors.Wrap(err, "Could not update unknown branch eni status")
	}

	// Populate branch eni attachments with ENIs that are not in use
	_, err = tx.ExecContext(ctx, "INSERT INTO branch_eni_attachments(branch_eni, state) SELECT branch_eni, 'unattached' FROM known_branch_enis WHERE  state = 'available' ON CONFLICT (branch_eni) DO UPDATE SET state = 'unattached'")
	if err != nil {
		return errors.Wrap(err, "Could not update unknown branch eni status")
	}

	_, err = tx.ExecContext(ctx, "INSERT INTO branch_eni_attachments(branch_eni, trunk_eni, idx, association_id, state) SELECT branch_eni, trunk_eni, idx, association_id, 'attached' FROM known_branch_eni_attachments ON CONFLICT(branch_eni) DO UPDATE SET trunk_eni = excluded.trunk_eni, state = 'attached', idx = excluded.idx, association_id = excluded.association_id")
	if err != nil {
		return errors.Wrap(err, "Could not insert new branch eni attachments")
	}

	return nil
}

type regionAccount struct {
	accountID string
	region    string
}

func (vpcService *vpcService) getRegionAccounts(ctx context.Context) ([]regionAccount, error) {
	tx, err := vpcService.db.BeginTx(ctx, &sql.TxOptions{
		ReadOnly: true,
	})
	if err != nil {
		logger.G(ctx).WithError(err).Error("Could not start database transaction")
		return nil, err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	rows, err := tx.QueryContext(ctx, "SELECT SUBSTR(availability_zone, 0, length(availability_zone)) AS region, account FROM account_mapping GROUP BY region, account")
	if err != nil {
		return nil, err
	}

	ret := []regionAccount{}
	for rows.Next() {
		var ra regionAccount
		err = rows.Scan(&ra.region, &ra.accountID)
		if err != nil {
			return nil, err
		}
		ret = append(ret, ra)
	}

	_ = tx.Commit()
	return ret, nil
}
