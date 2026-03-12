import React, { useEffect } from 'react';
import { useTranslation } from 'react-i18next';

import { useDispatch, useSelector } from 'react-redux';

import { Form } from './Form';

import Card from '../../ui/Card';
import { getBlockedServices, getAllBlockedServices, updateBlockedServices } from '../../../actions/services';

import PageTitle from '../../ui/PageTitle';

import { ScheduleForm } from './ScheduleForm';
import { RootState } from '../../../initialState';
import ServiceUrls from './ServiceUrls';

const getInitialDataForServices = (initial: any) => {
    // 将后端返回的 ids（可能为 null/[]/string[]）统一转换为
    // { blocked_services: { [id]: true } } 的形态，避免因 null 导致组件不渲染
    if (Array.isArray(initial)) {
        return initial.reduce(
            (acc: any, service: any) => {
                acc.blocked_services[service] = true;
                return acc;
            },
            { blocked_services: {} },
        );
    }
    // 对于 null 或未定义，返回空的 blocked_services
    return { blocked_services: {} };
};

const Services = () => {
    const [t] = useTranslation();
    const dispatch = useDispatch();

    const services = useSelector((state: RootState) => state.services);

    useEffect(() => {
        dispatch(getBlockedServices());
        dispatch(getAllBlockedServices());
    }, []);

    const handleSubmit = (values: any) => {
        if (!values || !values.blocked_services) {
            return;
        }

        // 仅从“已知服务列表”中收集被勾选的 ID，避免历史脏键（如 1password）混入
        const encodeKey = (id: string) => id.replace(/\./g, '__DOT__');
        const blocked_services = (services.allServices || [])
            .map((s: any) => s.id)
            .filter((id: string) => values.blocked_services?.[encodeKey(id)]);

        dispatch(
            updateBlockedServices({
                ids: blocked_services,
                schedule: services.list.schedule,
            }),
        );
    };

    const handleScheduleSubmit = (values: any) => {
        dispatch(
            updateBlockedServices({
                ids: services.list.ids || [],
                schedule: values,
            }),
        );
    };

    const initialValues = getInitialDataForServices(services.list.ids);

    return (
        <>
            <PageTitle title={t('blocked_services')} subtitle={t('blocked_services_desc')} />
            <ServiceUrls />
            <Card bodyType="card-body box-body--settings">
                <div className="form">
                    <Form
                        initialValues={initialValues}
                        blockedServices={services.allServices}
                        processing={services.processing}
                        processingSet={services.processingSet}
                        onSubmit={handleSubmit}
                    />
                </div>
            </Card>

            <Card
                title={t('schedule_services')}
                subtitle={t('schedule_services_desc')}
                bodyType="card-body box-body--settings"
            >
                <ScheduleForm schedule={services.list.schedule} onScheduleSubmit={handleScheduleSubmit} />
            </Card>
        </>
    );
};

export default Services;
